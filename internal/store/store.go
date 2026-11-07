// Package store is the data-access layer for tools, identities, check runs and reviews.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/mcpkg/mcpkg/internal/model"
)

// Store wraps the database.
type Store struct {
	DB  *sql.DB
	Log *slog.Logger // optional: Warn on query failures
}

// New returns a Store.
func New(db *sql.DB) *Store { return &Store{DB: db} }

// warn logs one query error at the boundary; the caller keeps the error.
func (s *Store) warn(op string, err error) {
	if s.Log != nil && err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, ErrNotFound) && !errors.Is(err, context.Canceled) {
		s.Log.Warn("store query failed", "op", op, "err", err)
	}
}

// ErrNotFound is returned when a tool does not exist.
var ErrNotFound = errors.New("not found")

const toolCols = `id, slug, name, title, description, version, repo_url, repo_subfolder, website_url,
	packages, remotes, sources, readme, readme_url, status, visibility, smoke_status,
	health, security, smoke, enrich, score, install, score_total, grade, stars, weekly_downloads,
	last_commit_at, published_at, verified_at, categories, summary, first_seen_at, updated_at`

// light columns skip the README for list pages.
const toolColsLight = `id, slug, name, title, description, version, repo_url, repo_subfolder, website_url,
	packages, remotes, sources, '' AS readme, readme_url, status, visibility, smoke_status,
	health, NULL::jsonb AS security, NULL::jsonb AS smoke, enrich, score, NULL::jsonb AS install, score_total, grade, stars, weekly_downloads,
	last_commit_at, published_at, verified_at, categories, summary, first_seen_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanTool(s scanner) (*model.Tool, error) {
	var t model.Tool
	var packages, remotes, sources []byte
	var health, security, smoke, enrich, score, install []byte
	var scoreTotal sql.NullInt64
	var lastCommit, published, verified sql.NullTime
	err := s.Scan(&t.ID, &t.Slug, &t.Name, &t.Title, &t.Description, &t.Version, &t.RepoURL, &t.RepoSubfolder, &t.WebsiteURL,
		&packages, &remotes, &sources, &t.Readme, &t.ReadmeURL, &t.Status, &t.Visibility, &t.SmokeStatus,
		&health, &security, &smoke, &enrich, &score, &install, &scoreTotal, &t.Grade, &t.Stars, &t.WeeklyDownloads,
		&lastCommit, &published, &verified, pq.Array(&t.Categories), &t.Summary, &t.FirstSeenAt, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal(packages, &t.Packages)
	_ = json.Unmarshal(remotes, &t.Remotes)
	_ = json.Unmarshal(sources, &t.Sources)
	decodeOpt(health, &t.Health)
	decodeOpt(security, &t.Security)
	decodeOpt(smoke, &t.Smoke)
	decodeOpt(enrich, &t.Enrich)
	decodeOpt(score, &t.Score)
	decodeOpt(install, &t.Install)
	if scoreTotal.Valid {
		v := int(scoreTotal.Int64)
		t.ScoreTotal = &v
	}
	t.LastCommitAt = nullTime(lastCommit)
	t.PublishedAt = nullTime(published)
	t.VerifiedAt = nullTime(verified)
	if t.Sources == nil {
		t.Sources = map[string]json.RawMessage{}
	}
	return &t, nil
}

func decodeOpt[T any](b []byte, dst **T) {
	if len(b) == 0 || string(b) == "null" {
		return
	}
	var v T
	if json.Unmarshal(b, &v) == nil {
		*dst = &v
	}
}

func nullTime(n sql.NullTime) *time.Time {
	if !n.Valid {
		return nil
	}
	t := n.Time
	return &t
}

// GetTool loads one tool by id.
func (s *Store) GetTool(ctx context.Context, id int64) (*model.Tool, error) {
	t, err := scanTool(s.DB.QueryRowContext(ctx, `SELECT `+toolCols+` FROM tools WHERE id=$1`, id))
	s.warn("get_tool", err)
	return t, err
}

// GetToolBySlug loads one tool by slug.
func (s *Store) GetToolBySlug(ctx context.Context, slug string) (*model.Tool, error) {
	return scanTool(s.DB.QueryRowContext(ctx, `SELECT `+toolCols+` FROM tools WHERE slug=$1`, slug))
}

// GetTools loads many tools by id, preserving the requested order.
func (s *Store) GetTools(ctx context.Context, ids []int64) ([]*model.Tool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+toolCols+` FROM tools WHERE id = ANY($1)`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[int64]*model.Tool{}
	for rows.Next() {
		t, err := scanTool(rows)
		if err != nil {
			return nil, err
		}
		byID[t.ID] = t
	}
	out := make([]*model.Tool, 0, len(ids))
	for _, id := range ids {
		if t, ok := byID[id]; ok {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

// Columns that may be updated by pipeline stages, and which are JSONB.
var updatable = map[string]bool{
	"title": true, "description": true, "version": true, "repo_url": true, "repo_subfolder": true, "website_url": true,
	"packages": true, "remotes": true, "readme": true, "readme_url": true, "status": true, "visibility": true,
	"smoke_status": true, "health": true, "security": true, "smoke": true, "enrich": true, "score": true,
	"install": true, "score_total": true, "grade": true, "stars": true, "weekly_downloads": true,
	"last_commit_at": true, "published_at": true, "verified_at": true, "categories": true, "summary": true,
}
var jsonCols = map[string]bool{"packages": true, "remotes": true, "health": true, "security": true, "smoke": true, "enrich": true, "score": true, "install": true}

// UpdateFields updates whitelisted columns on a tool.
func (s *Store) UpdateFields(ctx context.Context, ex execer, id int64, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if !updatable[k] {
			return fmt.Errorf("column %q is not updatable", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sets := make([]string, 0, len(keys)+1)
	args := make([]any, 0, len(keys)+1)
	for i, k := range keys {
		v := fields[k]
		ph := fmt.Sprintf("$%d", i+1)
		switch {
		case jsonCols[k]:
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			v = string(b)
			ph += "::jsonb"
		case k == "categories":
			if ss, ok := v.([]string); ok {
				if ss == nil {
					ss = []string{}
				}
				v = pq.Array(ss)
			}
		}
		sets = append(sets, k+"="+ph)
		args = append(args, v)
	}
	sets = append(sets, "updated_at=now()")
	args = append(args, id)
	q := fmt.Sprintf("UPDATE tools SET %s WHERE id=$%d", strings.Join(sets, ", "), len(args))
	if ex == nil {
		ex = s.DB
	}
	_, err := ex.ExecContext(ctx, q, args...)
	return err
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// StageRecord is what a pipeline stage persists.
type StageRecord struct {
	ToolID     int64
	Stage      string
	Decision   string
	Notes      []string
	Result     any
	Fields     map[string]any
	DurationMs int
	Worker     string
}

// SaveStage updates tool columns and appends a check_runs row in one transaction.
func (s *Store) SaveStage(ctx context.Context, r StageRecord) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.warn("save_stage.begin", err)
		return err
	}
	defer tx.Rollback()
	if err := s.UpdateFields(ctx, tx, r.ToolID, r.Fields); err != nil {
		return err
	}
	var res any
	if r.Result != nil {
		b, err := json.Marshal(r.Result)
		if err != nil {
			return err
		}
		res = string(b)
	}
	notes := r.Notes
	if notes == nil {
		notes = []string{}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO check_runs (tool_id, stage, decision, notes, result, duration_ms, worker)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7)`, r.ToolID, r.Stage, r.Decision, pq.Array(notes), res, r.DurationMs, r.Worker); err != nil {
		s.warn("save_stage.check_runs", err)
		return err
	}
	if err := tx.Commit(); err != nil {
		s.warn("save_stage.commit", err)
		return err
	}
	return nil
}

// CheckRun is one row of the audit trail.
type CheckRun struct {
	Stage      string
	Decision   string
	Notes      []string
	DurationMs int
	Worker     string
	CreatedAt  time.Time
}

// CheckRuns returns the latest runs for a tool.
func (s *Store) CheckRuns(ctx context.Context, toolID int64, limit int) ([]CheckRun, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT stage, decision, notes, duration_ms, worker, created_at
		FROM check_runs WHERE tool_id=$1 ORDER BY created_at DESC, id DESC LIMIT $2`, toolID, limit)
	if err != nil {
		s.warn("check_runs", err)
		return nil, err
	}
	defer rows.Close()
	var out []CheckRun
	for rows.Next() {
		var c CheckRun
		if err := rows.Scan(&c.Stage, &c.Decision, pq.Array(&c.Notes), &c.DurationMs, &c.Worker, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddReview opens a human review item (idempotent per tool+reason while open).
func (s *Store) AddReview(ctx context.Context, toolID int64, reason string, details any) error {
	var d any
	if details != nil {
		b, _ := json.Marshal(details)
		d = string(b)
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO review_queue (tool_id, reason, details) VALUES ($1,$2,$3::jsonb)
		ON CONFLICT (tool_id, reason) WHERE state='open' DO NOTHING`, toolID, reason, d)
	s.warn("add_review", err)
	return err
}

// ReviewItem is an open review.
type ReviewItem struct {
	ID        int64
	ToolID    int64
	Slug      string
	Name      string
	Reason    string
	Details   string
	CreatedAt time.Time
}

// OpenReviews lists open review items.
func (s *Store) OpenReviews(ctx context.Context, limit int) ([]ReviewItem, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT r.id, r.tool_id, t.slug, t.name, r.reason, coalesce(r.details::text,''), r.created_at
		FROM review_queue r JOIN tools t ON t.id=r.tool_id WHERE r.state='open' ORDER BY r.created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewItem
	for rows.Next() {
		var r ReviewItem
		if err := rows.Scan(&r.ID, &r.ToolID, &r.Slug, &r.Name, &r.Reason, &r.Details, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveReview closes a review item; "hidden" also hides the tool.
func (s *Store) ResolveReview(ctx context.Context, id int64, state, reviewer string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var toolID int64
	if err := tx.QueryRowContext(ctx, `UPDATE review_queue SET state=$2, reviewer=$3, resolved_at=now() WHERE id=$1 RETURNING tool_id`, id, state, reviewer).Scan(&toolID); err != nil {
		return err
	}
	if state == "hidden" {
		if _, err := tx.ExecContext(ctx, `UPDATE tools SET visibility='hidden', updated_at=now() WHERE id=$1`, toolID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Filter selects tool ids for batch enqueueing.
type Filter struct {
	All          bool
	Slugs        []string
	Status       string
	SmokeStatus  string
	MinStars     int
	NeverChecked bool
	StaleDays    int
	Limit        int
}

// SelectIDs returns tool ids matching a filter.
func (s *Store) SelectIDs(ctx context.Context, f Filter) ([]int64, error) {
	where := []string{"TRUE"}
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args))))
	}
	if len(f.Slugs) > 0 {
		add("slug = ANY(?)", pq.Array(f.Slugs))
	}
	if f.Status != "" {
		add("status = ?", f.Status)
	}
	if f.SmokeStatus != "" {
		add("smoke_status = ?", f.SmokeStatus)
	}
	if f.MinStars > 0 {
		add("stars >= ?", f.MinStars)
	}
	if f.NeverChecked {
		where = append(where, "verified_at IS NULL")
	}
	if f.StaleDays > 0 {
		add("(verified_at IS NULL OR verified_at < now() - make_interval(days => ?))", f.StaleDays)
	}
	if !f.All && len(f.Slugs) == 0 && f.Status == "" && f.SmokeStatus == "" && f.MinStars == 0 && !f.NeverChecked && f.StaleDays == 0 {
		return nil, errors.New("refusing to select every tool without --all")
	}
	q := "SELECT id FROM tools WHERE " + strings.Join(where, " AND ") + " ORDER BY stars DESC, id"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SelectStaleIDs finds listed tools whose re-check was lost (no queued/running job
// and no job activity within `days`). Used by the scheduler's self-healing.
func (s *Store) SelectStaleIDs(ctx context.Context, days, limit int) ([]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT t.id FROM tools t
		WHERE t.status NOT IN ('blocked','missing') AND t.visibility = 'listed'
		  AND (t.verified_at IS NULL OR t.verified_at < now() - make_interval(days => $1))
		  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.tool_id = t.id
		                  AND (j.state IN ('queued','running') OR j.updated_at > now() - make_interval(days => $1)))
		ORDER BY t.stars DESC, t.id LIMIT $2`, days, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SearchParams drives the search page and the JSON search API.
type SearchParams struct {
	Query       string
	Category    string
	MinGrade    string
	SmokeStatus string
	Sort        string // relevance | score | stars | recent
	Limit       int
	Offset      int
}

// Search returns listed tools matching the query.
func (s *Store) Search(ctx context.Context, p SearchParams) ([]*model.Tool, int, error) {
	if p.Limit <= 0 || p.Limit > 100 {
		p.Limit = 20
	}
	where := []string{"visibility = 'listed'"}
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	rank := "0"
	q := strings.TrimSpace(p.Query)
	if q != "" {
		tsq := arg(q)
		like := arg(q)
		where = append(where, fmt.Sprintf("(search @@ websearch_to_tsquery('english', %s) OR name %% %s OR name ILIKE '%%' || %s || '%%')", tsq, like, like))
		rank = fmt.Sprintf("(ts_rank_cd(search, websearch_to_tsquery('english', %s)) * 4 + similarity(name, %s))", tsq, like)
	}
	if p.Category != "" {
		where = append(where, fmt.Sprintf("%s = ANY(categories)", arg(p.Category)))
	}
	if p.SmokeStatus != "" {
		where = append(where, fmt.Sprintf("smoke_status = %s", arg(p.SmokeStatus)))
	}
	if g := strings.ToUpper(p.MinGrade); g != "" {
		grades := map[string][]string{"A": {"A"}, "B": {"A", "B"}, "C": {"A", "B", "C"}, "D": {"A", "B", "C", "D"}}
		if allowed, ok := grades[g]; ok {
			where = append(where, fmt.Sprintf("grade = ANY(%s)", arg(pq.Array(allowed))))
		}
	}
	order := "coalesce(score_total,0) DESC, stars DESC"
	switch p.Sort {
	case "stars":
		order = "stars DESC"
	case "recent":
		order = "last_commit_at DESC NULLS LAST"
	case "score":
	default:
		if q != "" {
			order = rank + " * (0.6 + coalesce(score_total,40)/250.0) DESC, stars DESC"
		}
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM tools WHERE "+cond, args...).Scan(&total); err != nil {
		s.warn("search.count", err)
		return nil, 0, err
	}
	limit, offset := arg(p.Limit), arg(p.Offset)
	rows, err := s.DB.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM tools WHERE %s ORDER BY %s, id LIMIT %s OFFSET %s", toolColsLight, cond, order, limit, offset), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*model.Tool
	for rows.Next() {
		t, err := scanTool(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// CategoryCount is a facet.
type CategoryCount struct {
	Category string
	Count    int
}

// CategoryCounts returns counts of listed tools per category.
func (s *Store) CategoryCounts(ctx context.Context) ([]CategoryCount, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT c, count(*) FROM tools, unnest(categories) c
		WHERE visibility='listed' GROUP BY c ORDER BY count(*) DESC, c`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CategoryCount
	for rows.Next() {
		var c CategoryCount
		if err := rows.Scan(&c.Category, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Stats are headline numbers for the home page and `mcpkg stats`.
type Stats struct {
	Total        int
	Listed       int
	Verified     int
	Works        int
	NeedsKey     int
	Broken       int
	Blocked      int
	Unmaintained int
	Pending      int
	BySource     map[string]int
}

// Stats computes catalog-wide counts.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.DB.QueryRowContext(ctx, `SELECT count(*),
		count(*) FILTER (WHERE visibility='listed'),
		count(*) FILTER (WHERE verified_at IS NOT NULL),
		count(*) FILTER (WHERE smoke_status='works'),
		count(*) FILTER (WHERE smoke_status IN ('needs_credentials','auth_required')),
		count(*) FILTER (WHERE smoke_status IN ('broken','install_failed')),
		count(*) FILTER (WHERE status='blocked'),
		count(*) FILTER (WHERE status='unmaintained'),
		count(*) FILTER (WHERE verified_at IS NULL)
		FROM tools`).Scan(&st.Total, &st.Listed, &st.Verified, &st.Works, &st.NeedsKey, &st.Broken, &st.Blocked, &st.Unmaintained, &st.Pending)
	if err != nil {
		s.warn("stats", err)
		return st, err
	}
	st.BySource = map[string]int{}
	rows, err := s.DB.QueryContext(ctx, `SELECT k, count(*) FROM tools, jsonb_object_keys(sources) k GROUP BY k`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return st, err
		}
		st.BySource[k] = n
	}
	return st, rows.Err()
}

// ListForAPI pages through listed tools for the subregistry API.
func (s *Store) ListForAPI(ctx context.Context, afterID int64, limit int, updatedSince *time.Time) ([]*model.Tool, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	args := []any{afterID, limit}
	cond := "visibility='listed' AND verified_at IS NOT NULL AND id > $1"
	if updatedSince != nil {
		args = append(args, *updatedSince)
		cond += " AND updated_at >= $3"
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+toolColsLight+` FROM tools WHERE `+cond+` ORDER BY id LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Tool
	for rows.Next() {
		t, err := scanTool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetCursor / SetCursor persist incremental crawl state.
func (s *Store) GetCursor(ctx context.Context, source string) (string, error) {
	var c string
	err := s.DB.QueryRowContext(ctx, `SELECT cursor FROM source_cursors WHERE source=$1`, source).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return c, err
}

// SetCursor stores the crawl cursor for a source.
func (s *Store) SetCursor(ctx context.Context, source, cursor string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO source_cursors (source, cursor, updated_at) VALUES ($1,$2,now())
		ON CONFLICT (source) DO UPDATE SET cursor=EXCLUDED.cursor, updated_at=now()`, source, cursor)
	return err
}

// RecordSubmission logs an author submission.
func (s *Store) RecordSubmission(ctx context.Context, url, clientKey string, toolID int64, state, errMsg string) error {
	var tid any
	if toolID > 0 {
		tid = toolID
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO submissions (url, tool_id, state, error, client_key) VALUES ($1,$2,$3,$4,$5)`,
		url, tid, state, errMsg, clientKey)
	s.warn("record_submission", err)
	return err
}

// SubmissionsLastHour counts all submissions in the last hour (global abuse cap).
func (s *Store) SubmissionsLastHour(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM submissions WHERE created_at > now() - interval '1 hour'`).Scan(&n)
	return n, err
}

// RecentSubmissions counts submissions from a client in the last hour (rate limiting).
func (s *Store) RecentSubmissions(ctx context.Context, clientKey string) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM submissions WHERE client_key=$1 AND created_at > now() - interval '1 hour'`, clientKey).Scan(&n)
	return n, err
}

// BenchmarkRow is one tool_benchmarks row, served to the Benchmarks tab and
// the /api/bench endpoint.
type BenchmarkRow struct {
	Scanner        string
	ScannerVersion string
	ImageDigest    string
	RanAt          time.Time
	DurationMs     int64
	ExitCode       int
	Verdict        string
	SkipReason     string
	Raw            json.RawMessage
	RunID          int64
}

// SaveBenchmarks appends rows for a run. Worker runs (runID 0) replace the
// tool's previous unnumbered rows first, so re-running a tool never duplicates
// results (gate B3); numbered gate runs keep their history.
func (s *Store) SaveBenchmarks(ctx context.Context, toolID int64, runID int64, rows []BenchmarkRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.warn("save_benchmarks.begin", err)
		return err
	}
	defer tx.Rollback()
	if runID == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM tool_benchmarks WHERE tool_id=$1 AND run_id IS NULL`, toolID); err != nil {
			s.warn("save_benchmarks.delete", err)
			return err
		}
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO tool_benchmarks
		(tool_id, scanner, scanner_version, image_digest, ran_at, duration_ms, exit_code, verdict, skip_reason, raw, run_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11)`)
	if err != nil {
		s.warn("save_benchmarks.prepare", err)
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		var raw any
		if len(r.Raw) > 0 {
			raw = string(r.Raw)
		}
		if _, err := stmt.ExecContext(ctx, toolID, r.Scanner, r.ScannerVersion, r.ImageDigest, r.RanAt,
			r.DurationMs, r.ExitCode, r.Verdict, r.SkipReason, raw, nullInt(runID)); err != nil {
			s.warn("save_benchmarks.insert", err)
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		s.warn("save_benchmarks.commit", err)
		return err
	}
	return nil
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// BenchmarksForTool returns the newest result per scanner for a tool.
func (s *Store) BenchmarksForTool(ctx context.Context, toolID int64) ([]BenchmarkRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT ON (scanner)
		scanner, scanner_version, image_digest, ran_at, duration_ms, exit_code, verdict, skip_reason, coalesce(raw::text,''), coalesce(run_id,0)
		FROM tool_benchmarks WHERE tool_id=$1 ORDER BY scanner, ran_at DESC`, toolID)
	if err != nil {
		s.warn("benchmarks_for_tool", err)
		return nil, err
	}
	defer rows.Close()
	var out []BenchmarkRow
	for rows.Next() {
		var r BenchmarkRow
		var raw string
		if err := rows.Scan(&r.Scanner, &r.ScannerVersion, &r.ImageDigest, &r.RanAt, &r.DurationMs,
			&r.ExitCode, &r.Verdict, &r.SkipReason, &raw, &r.RunID); err != nil {
			s.warn("benchmarks_for_tool.scan", err)
			return nil, err
		}
		r.Raw = json.RawMessage(raw)
		out = append(out, r)
	}
	return out, rows.Err()
}

// StartBenchRun opens a gate/report run row.
func (s *Store) StartBenchRun(ctx context.Context, sha string) (int64, error) {
	var id int64
	err := s.DB.QueryRowContext(ctx, `INSERT INTO bench_runs (sha) VALUES ($1) RETURNING id`, sha).Scan(&id)
	s.warn("start_bench_run", err)
	return id, err
}

// FinishBenchRun stores the gate report.
func (s *Store) FinishBenchRun(ctx context.Context, id int64, gate any) error {
	b, _ := json.Marshal(gate)
	_, err := s.DB.ExecContext(ctx, `UPDATE bench_runs SET finished_at=now(), gate=$2::jsonb WHERE id=$1`, id, string(b))
	s.warn("finish_bench_run", err)
	return err
}
