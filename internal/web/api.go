package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mcpkg/mcpkg/internal/identity"
	"github.com/mcpkg/mcpkg/internal/model"
)

// ---- badge ----

// badge serves /badge/<slug>.svg, a shields-style "mcpkg | A 91" badge that
// authors can put in their README (free distribution, like Codecov badges).
func (s *Server) badge(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSuffix(r.PathValue("file"), ".svg")
	label := s.Cfg.SiteName
	value, color := "not found", "#9f9f9f"
	if t, err := s.Store.GetToolBySlug(r.Context(), slug); err == nil {
		switch {
		case t.Visibility != "listed":
			value, color = "hidden", "#9f9f9f"
		case t.Score == nil:
			value, color = "checking", "#9f9f9f"
		default:
			value = fmt.Sprintf("%s %d", t.Score.Grade, t.Score.Total)
			color = map[string]string{"A": "#2e7d32", "B": "#558b2f", "C": "#b28704", "D": "#e65100", "F": "#c62828"}[t.Score.Grade]
			if t.SmokeStatus == "works" {
				value += " · works"
			}
		}
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(BadgeSVG(label, value, color)))
}

// BadgeSVG renders a flat badge. Text width is estimated (Verdana 11px ≈ 6.5px/char).
func BadgeSVG(label, value, color string) string {
	lw := 10 + int(float64(utf8.RuneCountInString(label))*6.5)
	vw := 10 + int(float64(utf8.RuneCountInString(value))*6.5)
	esc := func(s string) string {
		return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s: %s">
<title>%s: %s</title>
<linearGradient id="s" x2="0" y2="100%%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>
<clipPath id="r"><rect width="%d" height="20" rx="3" fill="#fff"/></clipPath>
<g clip-path="url(#r)"><rect width="%d" height="20" fill="#555"/><rect x="%d" width="%d" height="20" fill="%s"/><rect width="%d" height="20" fill="url(#s)"/></g>
<g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
<text x="%d" y="15" fill="#010101" fill-opacity=".3">%s</text><text x="%d" y="14">%s</text>
<text x="%d" y="15" fill="#010101" fill-opacity=".3">%s</text><text x="%d" y="14">%s</text></g></svg>`,
		lw+vw, esc(label), esc(value), esc(label), esc(value), lw+vw, lw, lw, vw, color, lw+vw,
		lw/2, esc(label), lw/2, esc(label), lw+vw/2, esc(value), lw+vw/2, esc(value))
}

// ---- subregistry (MCP Registry OpenAPI-compatible) ----

const serverSchema = "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json"

// registryName returns the official name, or a namespaced one we derive.
func (s *Server) registryName(t *model.Tool) string {
	if t.RegistryStatus() != "" {
		return t.Name
	}
	if repo, ok := identity.ParseRepo(t.RepoURL); ok && repo.IsGitHub() {
		n := "io.github." + repo.Owner + "/" + repo.Name
		if repo.Subfolder != "" {
			n += "-" + identity.ServerKey(repo.Subfolder)
		}
		return n
	}
	return s.Cfg.MetaNamespace + "/" + t.Slug
}

func (s *Server) serverEntry(t *model.Tool) map[string]any {
	srv := map[string]any{
		"$schema":     serverSchema,
		"name":        s.registryName(t),
		"description": firstNonEmpty(t.Summary, t.Description),
		"version":     firstNonEmpty(t.Version, "0.0.0"),
	}
	if t.Title != "" {
		srv["title"] = t.Title
	}
	if t.RepoURL != "" {
		repo := map[string]any{"url": t.RepoURL, "source": "github"}
		if t.RepoSubfolder != "" {
			repo["subfolder"] = t.RepoSubfolder
		}
		srv["repository"] = repo
	}
	if t.WebsiteURL != "" {
		srv["websiteUrl"] = t.WebsiteURL
	}
	if len(t.Packages) > 0 {
		pk := make([]model.Package, len(t.Packages))
		copy(pk, t.Packages)
		for i := range pk {
			pk[i].Bin = "" // not part of the official schema
		}
		srv["packages"] = pk
	}
	if len(t.Remotes) > 0 {
		srv["remotes"] = t.Remotes
	}
	trust := map[string]any{
		"smokeStatus": t.SmokeStatus, "status": t.Status, "verifiedAt": t.VerifiedAt,
		"pageUrl": s.Cfg.SiteURL + "/tool/" + t.Slug, "installMdUrl": s.Cfg.SiteURL + "/tool/" + t.Slug + "/install.md",
		"categories": t.Categories, "stars": t.Stars, "weeklyDownloads": t.WeeklyDownloads,
	}
	if t.Score != nil {
		trust["score"], trust["grade"] = t.Score.Total, t.Score.Grade
		trust["parts"] = map[string]int{"maintenance": t.Score.Maintenance, "security": t.Score.Security, "works": t.Score.Works, "community": t.Score.Community}
	}
	if t.Health != nil {
		trust["lastCommitAt"], trust["archived"], trust["openPullRequests"] = t.Health.LastCommitAt, t.Health.Archived, t.Health.OpenPRs
	}
	return map[string]any{"server": srv, "_meta": map[string]any{s.Cfg.MetaNamespace + "/trust": trust}}
}

func (s *Server) subregistryList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("cursor"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	var since *time.Time
	if v := q.Get("updated_since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			since = &t
		}
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	tools, err := s.Store.ListForAPI(r.Context(), after, limit, since)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "list failed"})
		return
	}
	servers := make([]map[string]any, 0, len(tools))
	next := ""
	for _, t := range tools {
		servers = append(servers, s.serverEntry(t))
		next = strconv.FormatInt(t.ID, 10)
	}
	if len(tools) < limit {
		next = "" // last page
	}
	writeJSON(w, 200, map[string]any{"servers": servers, "metadata": map[string]any{"count": len(servers), "nextCursor": next}})
}

func (s *Server) subregistryGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// Accept our slug or the registry-style name.
	t, err := s.Store.GetToolBySlug(r.Context(), name)
	if err != nil {
		t, err = s.Store.GetToolBySlug(r.Context(), identity.Slugify(name))
	}
	if err != nil || t.Visibility != "listed" {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	if v := r.PathValue("version"); v != "latest" && v != t.Version {
		writeJSON(w, 404, map[string]string{"error": "only the latest version is tracked"})
		return
	}
	writeJSON(w, 200, s.serverEntry(t))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
