// Package submit turns an author-provided link (GitHub repo, npm/PyPI page, or a
// remote MCP endpoint) into a candidate and pushes it through the pipeline.
// This is the "register your repo link" path; publishing to the official MCP
// Registry is picked up automatically within an hour as well.
package submit

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/mcpkg/mcpkg/internal/config"
	"github.com/mcpkg/mcpkg/internal/discover"
	"github.com/mcpkg/mcpkg/internal/httpx"
	"github.com/mcpkg/mcpkg/internal/identity"
	"github.com/mcpkg/mcpkg/internal/model"
	"github.com/mcpkg/mcpkg/internal/sources"
)

// Resolver resolves links to candidates.
type Resolver struct {
	Cfg  config.Config
	HTTP *httpx.Client
	Disc *discover.Discoverer
}

// ErrUnsupported is returned for links we cannot interpret.
var ErrUnsupported = errors.New("unsupported link: use a GitHub repo, npmjs.com/pypi.org package page, or an https MCP endpoint")

var pyprojectName = regexp.MustCompile(`(?m)^\s*name\s*=\s*["']([^"']+)["']`)

// Resolve builds a candidate from a link.
func (r *Resolver) Resolve(ctx context.Context, raw string) (sources.Candidate, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return sources.Candidate{}, ErrUnsupported
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch host {
	case "npmjs.com":
		if len(parts) >= 2 && parts[0] == "package" {
			name := strings.Join(parts[1:], "/")
			if strings.HasPrefix(name, "@") && len(parts) >= 3 {
				name = parts[1] + "/" + parts[2]
			} else {
				name = parts[1]
			}
			return r.npmCandidate(ctx, name)
		}
	case "pypi.org":
		if len(parts) >= 2 && parts[0] == "project" {
			return r.pypiCandidate(ctx, parts[1])
		}
	case "github.com", "gitlab.com", "codeberg.org", "bitbucket.org":
		repo, ok := identity.ParseRepo(raw)
		if !ok {
			return sources.Candidate{}, ErrUnsupported
		}
		return r.repoCandidate(ctx, repo)
	default:
		if u.Scheme != "https" {
			return sources.Candidate{}, errors.New("remote MCP endpoints must use https")
		}
		kind := "streamable-http"
		if strings.HasSuffix(u.Path, "/sse") {
			kind = "sse"
		}
		return sources.Candidate{Source: "submit", Name: host + strings.TrimRight(u.Path, "/"),
			Remotes: []model.Remote{{Type: kind, URL: raw}}, Raw: map[string]any{"url": raw}}, nil
	}
	return sources.Candidate{}, ErrUnsupported
}

func (r *Resolver) npmCandidate(ctx context.Context, name string) (sources.Candidate, error) {
	doc, err := sources.FetchNpmLatest(ctx, r.HTTP, r.Cfg.NpmRegistry, name)
	if err != nil {
		return sources.Candidate{}, fmt.Errorf("npm package %q not found: %w", name, err)
	}
	repoURL, dir := doc.RepoInfo()
	return sources.Candidate{Source: "npm", Name: doc.Name, Description: doc.Description, Version: doc.Version,
		RepoURL: repoURL, RepoSubfolder: dir, WebsiteURL: doc.Homepage,
		Packages: []model.Package{{RegistryType: "npm", Identifier: doc.Name, Version: doc.Version, Transport: model.Transport{Type: "stdio"}}},
		Raw:      map[string]any{"name": doc.Name, "version": doc.Version, "mcpDependency": doc.MCPDep(), "bins": doc.Bins(), "submitted": true}}, nil
}

func (r *Resolver) pypiCandidate(ctx context.Context, name string) (sources.Candidate, error) {
	p, err := sources.FetchPyPI(ctx, r.HTTP, r.Cfg.PyPIURL, name, "")
	if err != nil {
		return sources.Candidate{}, fmt.Errorf("PyPI project %q not found: %w", name, err)
	}
	return sources.Candidate{Source: "pypi", Name: p.Info.Name, Description: p.Info.Summary, Version: p.Info.Version,
		RepoURL: p.RepoURL(), WebsiteURL: p.Info.HomePage, PublishedAt: p.UploadTime(),
		Packages: []model.Package{{RegistryType: "pypi", Identifier: p.Info.Name, Version: p.Info.Version, Transport: model.Transport{Type: "stdio"}}},
		Raw:      map[string]any{"name": p.Info.Name, "version": p.Info.Version, "mcpDependency": p.MCPDep(), "submitted": true}}, nil
}

// repoCandidate looks for a published npm/PyPI package in the repo so the
// smoke test has something to install; otherwise it lists the repo as source-only.
func (r *Resolver) repoCandidate(ctx context.Context, repo identity.Repo) (sources.Candidate, error) {
	c := sources.Candidate{Source: "submit", Name: repo.Slug(), RepoURL: repo.URL(), RepoSubfolder: repo.Subfolder,
		Raw: map[string]any{"repo": repo.Key(), "submitted": true}}
	if !repo.IsGitHub() {
		return c, nil
	}
	prefix := r.Cfg.RawGitHub + "/" + repo.Owner + "/" + repo.Name + "/HEAD/"
	if repo.Subfolder != "" {
		prefix += repo.Subfolder + "/"
	}
	if data, _, err := r.HTTP.Do(ctx, httpx.Request{URL: prefix + "package.json", MaxBody: 1 << 20}); err == nil {
		var pkg struct {
			Name    string `json:"name"`
			Private bool   `json:"private"`
		}
		if jsonUnmarshal(data, &pkg) == nil && pkg.Name != "" && !pkg.Private {
			if nc, err := r.npmCandidate(ctx, pkg.Name); err == nil {
				nc.RepoURL, nc.RepoSubfolder = repo.URL(), repo.Subfolder
				return nc, nil
			}
		}
	}
	if data, _, err := r.HTTP.Do(ctx, httpx.Request{URL: prefix + "pyproject.toml", MaxBody: 1 << 20}); err == nil {
		if m := pyprojectName.FindSubmatch(data); m != nil {
			if pc, err := r.pypiCandidate(ctx, string(m[1])); err == nil {
				pc.RepoURL, pc.RepoSubfolder = repo.URL(), repo.Subfolder
				return pc, nil
			}
		}
	}
	return c, nil
}

// Submit resolves and upserts a link, returning the tool id.
func (r *Resolver) Submit(ctx context.Context, raw string) (discover.UpsertResult, sources.Candidate, error) {
	c, err := r.Resolve(ctx, raw)
	if err != nil {
		return discover.UpsertResult{}, c, err
	}
	res, err := r.Disc.Upsert(ctx, c)
	if err != nil {
		return res, c, err
	}
	if !res.Enqueued && !res.Created {
		// Known tool: force a fresh check (authors expect submissions to re-verify).
		_, _ = r.Disc.Queue.Enqueue(ctx, r.Disc.EntryStage, res.ToolID, zeroTime)
	}
	return res, c, nil
}
