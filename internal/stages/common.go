// Package stages implements the pipeline hooks: health, security, smoke, enrich, render.
// Rule of the house: only deterministic code decides decisions and scores.
// The LLM (enrich) writes descriptive text and nothing else.
package stages

import (
	"log/slog"
	"strings"
	"time"

	"github.com/mcpkg/mcpkg/internal/config"
	"github.com/mcpkg/mcpkg/internal/httpx"
	"github.com/mcpkg/mcpkg/internal/identity"
	"github.com/mcpkg/mcpkg/internal/model"
	"github.com/mcpkg/mcpkg/internal/pipeline"
	"github.com/mcpkg/mcpkg/internal/store"
)

// Deps are shared by all stages.
type Deps struct {
	Cfg   config.Config
	HTTP  *httpx.Client
	Store *store.Store
	Gates pipeline.Gates
	Pipe  pipeline.Config
	Log   *slog.Logger
	Now   func() time.Time
}

func (d *Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

func githubRepo(t *model.Tool) (identity.Repo, bool) {
	r, ok := identity.ParseRepo(t.RepoURL)
	if !ok || !r.IsGitHub() {
		return identity.Repo{}, false
	}
	r.Subfolder = identity.CleanSubfolder(t.RepoSubfolder)
	return r, true
}

func firstPackage(t *model.Tool, kind string) (model.Package, bool) {
	for _, p := range t.Packages {
		if p.RegistryType == kind {
			return p, true
		}
	}
	return model.Package{}, false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// avoid cutting a UTF-8 sequence in half
	for n > 0 && n < len(s) && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

func tailString(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && (s[i]&0xC0) == 0x80 {
		i++
	}
	return "…" + s[i:]
}

func ptrTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
