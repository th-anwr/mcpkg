// Package discover turns source candidates into deduplicated catalog entries.
//
// Every source's view of a tool is kept under tools.sources[<source>]; the
// public fields are recomposed from those views by source precedence
// (registry > npm/pypi > crates > github > submit), so re-crawls are idempotent.
package discover

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/mcpkg/mcpkg/internal/identity"
	"github.com/mcpkg/mcpkg/internal/model"
	"github.com/mcpkg/mcpkg/internal/queue"
	"github.com/mcpkg/mcpkg/internal/sources"
	"github.com/mcpkg/mcpkg/internal/store"
)

// Discoverer merges candidates and enqueues the first pipeline stage.
type Discoverer struct {
	Store      *store.Store
	Queue      *queue.Queue
	EntryStage string // usually "health"
	Log        *slog.Logger
}

// Stats summarizes one crawl.
type Stats struct {
	Source   string
	Seen     int
	Created  int
	Updated  int
	Merged   int
	Enqueued int
	Errors   int
}

// Run crawls one source (incrementally unless full) and upserts what it finds.
func (d *Discoverer) Run(ctx context.Context, src sources.Source, limit int, full bool) (Stats, error) {
	st := Stats{Source: src.Name()}
	since := ""
	if !full {
		var err error
		if since, err = d.Store.GetCursor(ctx, src.Name()); err != nil {
			return st, err
		}
	}
	next, err := src.Crawl(ctx, sources.Options{Limit: limit, Since: since}, func(c sources.Candidate) error {
		st.Seen++
		res, err := d.Upsert(ctx, c)
		if err != nil {
			st.Errors++
			d.Log.Warn("upsert failed", "source", c.Source, "name", c.Name, "err", err)
			return nil
		}
		switch {
		case res.Created:
			st.Created++
		case res.Changed:
			st.Updated++
		}
		if res.Merged > 0 {
			st.Merged += res.Merged
		}
		if res.Enqueued {
			st.Enqueued++
		}
		if st.Seen%200 == 0 {
			d.Log.Info("discovery progress", "source", src.Name(), "seen", st.Seen, "created", st.Created)
		}
		return nil
	})
	if err == nil && next != "" && limit == 0 {
		err = d.Store.SetCursor(ctx, src.Name(), next)
	}
	return st, err
}

// UpsertResult describes what happened to one candidate.
type UpsertResult struct {
	ToolID   int64
	Created  bool
	Changed  bool
	Merged   int
	Enqueued bool
}

// Key is one identity of a candidate.
type Key struct {
	Kind, Value string
	Strong      bool
}

// Keys computes the identities used for dedupe.
func Keys(c sources.Candidate) []Key {
	var keys []Key
	add := func(kind, value string, strong bool) {
		if value != "" {
			keys = append(keys, Key{kind, value, strong})
		}
	}
	if c.Source == "registry" {
		add("registry", strings.ToLower(c.Name), true)
	}
	for _, p := range c.Packages {
		switch p.RegistryType {
		case "npm":
			add("npm", identity.NpmName(p.Identifier), true)
		case "pypi":
			add("pypi", identity.PyPIName(p.Identifier), true)
		case "oci":
			add("oci", identity.OCIName(p.Identifier), true)
		case "crate":
			add("crate", strings.ToLower(p.Identifier), true)
		case "nuget":
			add("nuget", strings.ToLower(p.Identifier), true)
		}
	}
	for _, r := range c.Remotes {
		add("remote", identity.RemoteURL(r.URL), true)
	}
	if repo, ok := identity.ParseRepo(c.RepoURL); ok {
		if sub := identity.CleanSubfolder(c.RepoSubfolder); sub != "" {
			repo.Subfolder = sub
		}
		// A repo key is strong only when it pins a monorepo subfolder; a bare repo
		// key is weak because one repo can host many servers.
		add("repo", repo.Key(), repo.Subfolder != "")
	}
	return keys
}

// sourceView is what we persist per source under tools.sources.
type sourceView struct {
	Name          string          `json:"name"`
	Title         string          `json:"title,omitempty"`
	Description   string          `json:"description,omitempty"`
	Version       string          `json:"version,omitempty"`
	RepoURL       string          `json:"repoUrl,omitempty"`
	RepoSubfolder string          `json:"repoSubfolder,omitempty"`
	WebsiteURL    string          `json:"websiteUrl,omitempty"`
	Packages      []model.Package `json:"packages,omitempty"`
	Remotes       []model.Remote  `json:"remotes,omitempty"`
	Status        string          `json:"status,omitempty"`
	PublishedAt   *time.Time      `json:"publishedAt,omitempty"`
	UpdatedAt     *time.Time      `json:"updatedAt,omitempty"`
	Stars         int             `json:"stars,omitempty"`
	Raw           map[string]any  `json:"raw,omitempty"`
	SeenAt        time.Time       `json:"seenAt"`
}

var precedence = map[string]int{"registry": 50, "npm": 40, "pypi": 40, "crates": 30, "github": 20, "submit": 10}

// Upsert merges one candidate into the catalog.
func (d *Discoverer) Upsert(ctx context.Context, c sources.Candidate) (UpsertResult, error) {
	var res UpsertResult
	c.RepoSubfolder = identity.CleanSubfolder(c.RepoSubfolder)
	if repo, ok := identity.ParseRepo(c.RepoURL); ok {
		if c.RepoSubfolder == "" {
			c.RepoSubfolder = repo.Subfolder
		}
		c.RepoURL = repo.URL()
	} else {
		c.RepoURL = ""
	}
	keys := Keys(c)
	if len(keys) == 0 {
		return res, errors.New("candidate has no identity")
	}
	tx, err := d.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	// Serialize merges: discovery is cheap, correctness matters more than parallelism here.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(727001)`); err != nil {
		return res, err
	}

	ids, err := matchStrong(ctx, tx, keys)
	if err != nil {
		return res, err
	}
	claimWeak := true
	if len(ids) == 0 {
		if weakID, ok, err := matchWeak(ctx, tx, keys, c); err != nil {
			return res, err
		} else if ok {
			ids = []int64{weakID}
		} else if hasWeak(keys) {
			// Weak key exists but belongs to an unrelated tool from the same monorepo.
			claimWeak = !weakClaimed(ctx, tx, keys)
		}
	}

	var toolID int64
	var before *composed
	switch len(ids) {
	case 0:
		if toolID, err = createTool(ctx, tx, c); err != nil {
			return res, err
		}
		res.Created = true
	default:
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		toolID = ids[0]
		if len(ids) > 1 {
			if err := mergeTools(ctx, tx, toolID, ids[1:]); err != nil {
				return res, err
			}
			res.Merged = len(ids) - 1
		}
		if before, err = loadComposed(ctx, tx, toolID); err != nil {
			return res, err
		}
	}
	res.ToolID = toolID

	// Store this source's view.
	view := sourceView{
		Name: c.Name, Title: c.Title, Description: c.Description, Version: c.Version,
		RepoURL: c.RepoURL, RepoSubfolder: c.RepoSubfolder, WebsiteURL: c.WebsiteURL,
		Packages: c.Packages, Remotes: c.Remotes, Status: c.Status,
		PublishedAt: c.PublishedAt, UpdatedAt: c.UpdatedAt, Stars: c.Stars, Raw: c.Raw, SeenAt: time.Now().UTC(),
	}
	vb, _ := json.Marshal(view)
	if _, err := tx.ExecContext(ctx, `UPDATE tools SET sources = jsonb_set(sources, ARRAY[$2::text], $3::jsonb, true) WHERE id=$1`,
		toolID, c.Source, string(vb)); err != nil {
		return res, err
	}
	for _, k := range keys {
		if !k.Strong && !claimWeak {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO identities (kind, value, tool_id) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, k.Kind, k.Value, toolID); err != nil {
			return res, err
		}
	}
	after, err := recompose(ctx, tx, toolID)
	if err != nil {
		return res, err
	}
	res.Changed = res.Created || before == nil || before.materialChange(after)
	if err := tx.Commit(); err != nil {
		return res, err
	}
	if res.Changed && after.Status != "blocked" && d.EntryStage != "" {
		res.Enqueued, err = d.Queue.Enqueue(ctx, d.EntryStage, toolID, time.Time{})
	}
	return res, err
}

func matchStrong(ctx context.Context, tx *sql.Tx, keys []Key) ([]int64, error) {
	set := map[int64]bool{}
	for _, k := range keys {
		if !k.Strong {
			continue
		}
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT tool_id FROM identities WHERE kind=$1 AND value=$2`, k.Kind, k.Value).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		set[id] = true
	}
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out, nil
}

func hasWeak(keys []Key) bool {
	for _, k := range keys {
		if !k.Strong {
			return true
		}
	}
	return false
}

func weakClaimed(ctx context.Context, tx *sql.Tx, keys []Key) bool {
	for _, k := range keys {
		if k.Strong {
			continue
		}
		var id int64
		if tx.QueryRowContext(ctx, `SELECT tool_id FROM identities WHERE kind=$1 AND value=$2`, k.Kind, k.Value).Scan(&id) == nil {
			return true
		}
	}
	return false
}

// matchWeak merges via a bare repo key only when it cannot collide: the existing
// tool must not already own a different package in the same ecosystem.
func matchWeak(ctx context.Context, tx *sql.Tx, keys []Key, c sources.Candidate) (int64, bool, error) {
	for _, k := range keys {
		if k.Strong {
			continue
		}
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT tool_id FROM identities WHERE kind=$1 AND value=$2`, k.Kind, k.Value).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, false, err
		}
		if len(c.Packages) == 0 && len(c.Remotes) == 0 && c.Source != "registry" {
			return id, true, nil // e.g. a GitHub-search hit for a repo we already know
		}
		for _, p := range c.Packages {
			kind := map[string]string{"npm": "npm", "pypi": "pypi", "oci": "oci", "crate": "crate"}[p.RegistryType]
			if kind == "" {
				continue
			}
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM identities WHERE tool_id=$1 AND kind=$2`, id, kind).Scan(&n); err != nil {
				return 0, false, err
			}
			if n > 0 {
				return 0, false, nil // the repo already maps to another package: different server
			}
		}
		var regs int
		if c.Source == "registry" {
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM identities WHERE tool_id=$1 AND kind='registry'`, id).Scan(&regs); err != nil {
				return 0, false, err
			}
			if regs > 0 {
				return 0, false, nil
			}
		}
		return id, true, nil
	}
	return 0, false, nil
}

func createTool(ctx context.Context, tx *sql.Tx, c sources.Candidate) (int64, error) {
	base := identity.Slugify(primaryName(c))
	slug := base
	for i := 2; ; i++ {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tools WHERE slug=$1)`, slug).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			break
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO tools (slug, name) VALUES ($1,$2) RETURNING id`, slug, primaryName(c)).Scan(&id)
	return id, err
}

func primaryName(c sources.Candidate) string {
	if c.Name != "" {
		return c.Name
	}
	if repo, ok := identity.ParseRepo(c.RepoURL); ok {
		return repo.Slug()
	}
	return "unnamed"
}

func mergeTools(ctx context.Context, tx *sql.Tx, keep int64, drop []int64) error {
	for _, id := range drop {
		if _, err := tx.ExecContext(ctx, `UPDATE tools k SET sources = d.sources || k.sources FROM tools d WHERE k.id=$1 AND d.id=$2`, keep, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE identities SET tool_id=$1 WHERE tool_id=$2`, keep, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tools WHERE id=$1`, id); err != nil {
			return err
		}
	}
	return nil
}

// composed is the public view derived from all source views.
type composed struct {
	Name, Title, Description, Version, RepoURL, RepoSubfolder, WebsiteURL, Status string
	Packages                                                                      []model.Package
	Remotes                                                                       []model.Remote
	PublishedAt                                                                   *time.Time
	Stars                                                                         int
}

func (b *composed) materialChange(a *composed) bool {
	return b.Version != a.Version || b.RepoURL != a.RepoURL || b.RepoSubfolder != a.RepoSubfolder ||
		!reflect.DeepEqual(b.Packages, a.Packages) || !reflect.DeepEqual(b.Remotes, a.Remotes) || b.Status != a.Status
}

func loadViews(ctx context.Context, tx *sql.Tx, id int64) (map[string]sourceView, error) {
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT sources FROM tools WHERE id=$1`, id).Scan(&raw); err != nil {
		return nil, err
	}
	views := map[string]sourceView{}
	if err := json.Unmarshal(raw, &views); err != nil {
		return nil, err
	}
	return views, nil
}

func loadComposed(ctx context.Context, tx *sql.Tx, id int64) (*composed, error) {
	views, err := loadViews(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return compose(views), nil
}

// Compose merges source views by precedence (exported for tests).
func compose(views map[string]sourceView) *composed {
	names := make([]string, 0, len(views))
	for k := range views {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		if precedence[names[i]] != precedence[names[j]] {
			return precedence[names[i]] > precedence[names[j]]
		}
		return names[i] < names[j]
	})
	c := &composed{}
	pkgSeen := map[string]bool{}
	remSeen := map[string]bool{}
	for _, n := range names {
		v := views[n]
		set := func(dst *string, val string) {
			if *dst == "" && strings.TrimSpace(val) != "" {
				*dst = strings.TrimSpace(val)
			}
		}
		set(&c.Name, v.Name)
		set(&c.Title, v.Title)
		set(&c.Description, v.Description)
		set(&c.Version, v.Version)
		if c.RepoURL == "" && v.RepoURL != "" {
			c.RepoURL, c.RepoSubfolder = v.RepoURL, v.RepoSubfolder
		}
		set(&c.WebsiteURL, v.WebsiteURL)
		if n == "registry" {
			c.Status = v.Status
		}
		for _, p := range v.Packages {
			k := p.RegistryType + ":" + strings.ToLower(p.Identifier)
			if !pkgSeen[k] {
				pkgSeen[k] = true
				c.Packages = append(c.Packages, p)
			}
		}
		for _, r := range v.Remotes {
			k := identity.RemoteURL(r.URL)
			if !remSeen[k] {
				remSeen[k] = true
				c.Remotes = append(c.Remotes, r)
			}
		}
		if c.PublishedAt == nil && v.PublishedAt != nil {
			c.PublishedAt = v.PublishedAt
		}
		if v.Stars > c.Stars {
			c.Stars = v.Stars
		}
	}
	return c
}

func recompose(ctx context.Context, tx *sql.Tx, id int64) (*composed, error) {
	views, err := loadViews(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	c := compose(views)
	pk, _ := json.Marshal(nonNilPackages(c.Packages))
	rm, _ := json.Marshal(nonNilRemotes(c.Remotes))
	status := ""
	switch c.Status {
	case "deleted":
		status = "blocked" // removed by registry moderation (spam/malware)
	case "deprecated":
		status = "deprecated"
	}
	_, err = tx.ExecContext(ctx, `UPDATE tools SET
		name = CASE WHEN $2 <> '' THEN $2 ELSE name END,
		title=$3, description=$4, version=$5, repo_url=$6, repo_subfolder=$7, website_url=$8,
		packages=$9::jsonb, remotes=$10::jsonb,
		published_at = COALESCE($11, published_at),
		stars = GREATEST(stars, $12),
		status = CASE WHEN $13 <> '' THEN $13 WHEN status = 'deprecated' AND $14 THEN 'new' ELSE status END,
		visibility = CASE WHEN $13 = 'blocked' THEN 'hidden' ELSE visibility END,
		updated_at=now()
		WHERE id=$1`,
		id, c.Name, c.Title, c.Description, c.Version, c.RepoURL, c.RepoSubfolder, c.WebsiteURL,
		string(pk), string(rm), c.PublishedAt, c.Stars, status, c.Status == "active")
	if err != nil {
		return nil, err
	}
	if status != "" {
		c.Status = status
	}
	return c, nil
}

func nonNilPackages(p []model.Package) []model.Package {
	if p == nil {
		return []model.Package{}
	}
	return p
}

func nonNilRemotes(r []model.Remote) []model.Remote {
	if r == nil {
		return []model.Remote{}
	}
	return r
}
