// Package web serves the npm-style site, agent install pages, the JSON API,
// a subregistry-compatible API (MCP Registry OpenAPI + _meta), and badges.
//
// Pages are a single template per page type filled from the database, so a new
// tool never needs a hand-written page: the pipeline fills the data, the
// template renders it.
package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	ttemplate "text/template"
	"time"

	"github.com/mcpkg/mcpkg/internal/config"
	"github.com/mcpkg/mcpkg/internal/model"
	"github.com/mcpkg/mcpkg/internal/store"
	"github.com/mcpkg/mcpkg/internal/submit"
)

//go:embed templates/*.html templates/*.tmpl
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server is the HTTP application.
type Server struct {
	Cfg      config.Config
	Store    *store.Store
	Resolver *submit.Resolver
	Log      *slog.Logger
	pages    map[string]*template.Template
	text     *ttemplate.Template
}

// New parses templates.
func New(cfg config.Config, st *store.Store, res *submit.Resolver, log *slog.Logger) (*Server, error) {
	s := &Server{Cfg: cfg, Store: st, Resolver: res, Log: log, pages: map[string]*template.Template{}}
	for _, page := range []string{"home", "search", "tool", "submit", "review", "error", "agents"} {
		t, err := template.New("layout.html").Funcs(funcs()).ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", page, err)
		}
		s.pages[page] = t
	}
	tt, err := ttemplate.New("install.md.tmpl").Funcs(funcs()).ParseFS(templateFS, "templates/install.md.tmpl", "templates/llms.txt.tmpl")
	if err != nil {
		return nil, fmt.Errorf("text templates: %w", err)
	}
	s.text = tt
	return s, nil
}

// Handler returns the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(http.FileServerFS(static), 24*time.Hour)))
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /search", s.search)
	mux.HandleFunc("GET /tool/{slug}", s.tool)
	mux.HandleFunc("GET /tool/{slug}/install.md", s.installMD)
	mux.HandleFunc("GET /api/tools/{slug}/manifest.json", s.manifestJSON)
	mux.HandleFunc("GET /api/tools/{slug}", s.toolJSON)
	mux.HandleFunc("GET /api/search", s.searchJSON)
	mux.HandleFunc("GET /api/stats", s.statsJSON)
	mux.HandleFunc("GET /v0.1/servers", s.subregistryList)
	mux.HandleFunc("GET /v0.1/servers/{name}/versions/{version}", s.subregistryGet)
	mux.HandleFunc("GET /badge/{file}", s.badge)
	mux.HandleFunc("GET /llms.txt", s.llms)
	mux.HandleFunc("GET /agents", s.agents)
	mux.HandleFunc("GET /submit", s.submitForm)
	mux.HandleFunc("POST /submit", s.submitPost)
	mux.HandleFunc("GET /admin/review", s.adminOnly(s.review))
	mux.HandleFunc("POST /admin/review/{id}", s.adminOnly(s.reviewPost))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.Store.DB.PingContext(ctx); err != nil {
			http.Error(w, "db down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	return s.middleware(mux)
}

// imgHosts are the only hosts README images may load from (no tracking pixels
// from arbitrary servers). Images elsewhere show their alt text.
const imgHosts = "https://raw.githubusercontent.com https://*.githubusercontent.com https://github.com " +
	"https://img.shields.io https://badge.fury.io https://badgen.net https://codecov.io https://user-images.githubusercontent.com"

// Submission caps: per client and for the whole site, per hour.
const (
	submitPerClientHour = 10
	submitGlobalHour    = 300
)

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: "+imgHosts+"; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; script-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/v0.1/") || r.URL.Path == "/llms.txt" || strings.HasSuffix(r.URL.Path, "/install.md") {
			h.Set("Access-Control-Allow-Origin", "*")
		}
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rec, r)
		if s.Log != nil && r.URL.Path != "/healthz" && !strings.HasPrefix(r.URL.Path, "/static/") {
			s.Log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rec.code, "ms", time.Since(start).Milliseconds())
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) { r.code = code; r.ResponseWriter.WriteHeader(code) }

func cacheFor(h http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(d.Seconds())))
		h.ServeHTTP(w, r)
	})
}

// pageData is shared by all HTML templates.
type pageData struct {
	Site        string
	SiteURL     string
	Title       string
	Description string
	Canonical   string
	Query       string
	Nav         string

	Tool       *model.Tool
	ReadmeHTML template.HTML
	Runs       []store.CheckRun
	Tab        string
	Method     *model.InstallMethod

	Results    []*model.Tool
	Total      int
	Params     store.SearchParams
	Categories []store.CategoryCount
	PrevPage   string
	NextPage   string

	Stats  store.Stats
	Top    []*model.Tool
	Recent []*model.Tool

	Message string
	Error   string
	Form    string
	Reviews []store.ReviewItem
	Status  int
}

func (s *Server) base(title, desc string) pageData {
	return pageData{Site: s.Cfg.SiteName, SiteURL: s.Cfg.SiteURL, Title: title, Description: desc}
}

func (s *Server) render(w http.ResponseWriter, page string, d pageData) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "unknown page", 500)
		return
	}
	var buf strings.Builder
	if err := t.ExecuteTemplate(&buf, "layout.html", d); err != nil {
		s.Log.Error("render", "page", page, "err", err)
		http.Error(w, "template error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if d.Status != 0 {
		w.WriteHeader(d.Status)
	}
	_, _ = w.Write([]byte(buf.String()))
}

func (s *Server) notFound(w http.ResponseWriter, msg string) {
	d := s.base("Not found", msg)
	d.Error, d.Status = msg, http.StatusNotFound
	s.render(w, "error", d)
}

func (s *Server) serverError(w http.ResponseWriter, err error) {
	s.Log.Error("request failed", "err", err)
	d := s.base("Something went wrong", "")
	d.Error, d.Status = "Something went wrong on our side. Please try again.", http.StatusInternalServerError
	s.render(w, "error", d)
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := s.base(s.Cfg.SiteName+": every open-source MCP server, tested", "Search open-source MCP servers with health, security and smoke-test results, plus exact install steps for any MCP client.")
	var err error
	if d.Stats, err = s.Store.Stats(ctx); err != nil {
		s.serverError(w, err)
		return
	}
	if d.Top, _, err = s.Store.Search(ctx, store.SearchParams{Sort: "score", Limit: 8, SmokeStatus: "works"}); err != nil {
		s.serverError(w, err)
		return
	}
	if d.Recent, _, err = s.Store.Search(ctx, store.SearchParams{Sort: "recent", Limit: 8}); err != nil {
		s.serverError(w, err)
		return
	}
	d.Categories, _ = s.Store.CategoryCounts(ctx)
	d.Nav = "home"
	s.render(w, "home", d)
}

func searchParams(r *http.Request) store.SearchParams {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	limit := 20
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 && l <= 100 {
		limit = l
	}
	return store.SearchParams{Query: strings.TrimSpace(q.Get("q")), Category: q.Get("category"), MinGrade: q.Get("grade"),
		SmokeStatus: q.Get("status"), Sort: q.Get("sort"), Limit: limit, Offset: (page - 1) * limit}
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	p := searchParams(r)
	res, total, err := s.Store.Search(r.Context(), p)
	if err != nil {
		s.serverError(w, err)
		return
	}
	title := "Browse MCP servers"
	if p.Query != "" {
		title = fmt.Sprintf("Results for “%s”", p.Query)
	}
	d := s.base(title, "")
	d.Results, d.Total, d.Params, d.Query, d.Nav = res, total, p, p.Query, "search"
	d.Categories, _ = s.Store.CategoryCounts(r.Context())
	page := p.Offset/p.Limit + 1
	link := func(n int) string {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(n))
		return "/search?" + q.Encode()
	}
	if page > 1 {
		d.PrevPage = link(page - 1)
	}
	if p.Offset+len(res) < total {
		d.NextPage = link(page + 1)
	}
	s.render(w, "search", d)
}

func (s *Server) loadTool(w http.ResponseWriter, r *http.Request) *model.Tool {
	t, err := s.Store.GetToolBySlug(r.Context(), r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, "No tool with that name.")
		return nil
	}
	if err != nil {
		s.serverError(w, err)
		return nil
	}
	if t.Visibility != "listed" {
		s.notFound(w, "This tool is hidden: it failed a security check or was removed by its publisher.")
		return nil
	}
	return t
}

func (s *Server) tool(w http.ResponseWriter, r *http.Request) {
	t := s.loadTool(w, r)
	if t == nil {
		return
	}
	d := s.base(t.DisplayName(), t.Summary)
	d.Tool, d.Canonical = t, s.Cfg.SiteURL+"/tool/"+t.Slug
	d.Tab = r.URL.Query().Get("tab")
	switch d.Tab {
	case "tools", "install", "health":
	default:
		d.Tab = "readme"
	}
	if d.Tab == "readme" {
		d.ReadmeHTML = RenderReadme(t)
	}
	if t.Install != nil && len(t.Install.Methods) > 0 {
		d.Method = &t.Install.Methods[0]
	}
	if d.Tab == "health" {
		d.Runs, _ = s.Store.CheckRuns(r.Context(), t.ID, 30)
	}
	s.render(w, "tool", d)
}

func (s *Server) installMD(w http.ResponseWriter, r *http.Request) {
	t := s.loadTool(w, r)
	if t == nil {
		return
	}
	if t.Install == nil {
		http.Error(w, "install instructions are not ready yet; this tool is still being checked", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	if err := s.text.ExecuteTemplate(w, "install.md.tmpl", map[string]any{"T": t, "M": t.Install, "Site": s.Cfg.SiteName, "SiteURL": s.Cfg.SiteURL}); err != nil {
		s.Log.Error("install.md", "err", err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *Server) manifestJSON(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.GetToolBySlug(r.Context(), r.PathValue("slug"))
	if err != nil || t.Visibility != "listed" {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	if t.Install == nil {
		writeJSON(w, 404, map[string]string{"error": "not verified yet"})
		return
	}
	writeJSON(w, 200, t.Install)
}

// publicTool is the JSON shape of /api/tools/{slug}.
type publicTool struct {
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Summary     string          `json:"summary"`
	Description string          `json:"description,omitempty"`
	Version     string          `json:"version,omitempty"`
	Repository  string          `json:"repository,omitempty"`
	Categories  []string        `json:"categories"`
	Score       *model.Score    `json:"score,omitempty"`
	SmokeStatus string          `json:"smokeStatus"`
	Status      string          `json:"status"`
	Stars       int             `json:"stars"`
	Downloads   int64           `json:"weeklyDownloads"`
	Tools       []string        `json:"tools,omitempty"`
	VerifiedAt  *time.Time      `json:"verifiedAt,omitempty"`
	PageURL     string          `json:"pageUrl"`
	InstallMD   string          `json:"installMdUrl"`
	Manifest    string          `json:"manifestUrl"`
	Packages    []model.Package `json:"packages"`
	Remotes     []model.Remote  `json:"remotes"`
}

func (s *Server) public(t *model.Tool) publicTool {
	p := publicTool{Slug: t.Slug, Name: t.Name, Title: t.Title, Summary: t.Summary, Version: t.Version, Repository: t.RepoURL,
		Categories: t.Categories, Score: t.Score, SmokeStatus: t.SmokeStatus, Status: t.Status, Stars: t.Stars,
		Downloads: t.WeeklyDownloads, VerifiedAt: t.VerifiedAt, Packages: t.Packages, Remotes: t.Remotes,
		PageURL: s.Cfg.SiteURL + "/tool/" + t.Slug, InstallMD: s.Cfg.SiteURL + "/tool/" + t.Slug + "/install.md",
		Manifest: s.Cfg.SiteURL + "/api/tools/" + t.Slug + "/manifest.json"}
	if t.Enrich != nil {
		p.Description = t.Enrich.Description
	}
	if t.Smoke != nil {
		for _, tool := range t.Smoke.Tools {
			p.Tools = append(p.Tools, tool.Name)
		}
	}
	return p
}

func (s *Server) toolJSON(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.GetToolBySlug(r.Context(), r.PathValue("slug"))
	if err != nil || t.Visibility != "listed" {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, 200, s.public(t))
}

func (s *Server) searchJSON(w http.ResponseWriter, r *http.Request) {
	p := searchParams(r)
	res, total, err := s.Store.Search(r.Context(), p)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "search failed"})
		return
	}
	out := make([]publicTool, 0, len(res))
	for _, t := range res {
		out = append(out, s.public(t))
	}
	writeJSON(w, 200, map[string]any{"total": total, "results": out})
}

func (s *Server) statsJSON(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.Stats(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "stats failed"})
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) llms(w http.ResponseWriter, r *http.Request) {
	st, _ := s.Store.Stats(r.Context())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_ = s.text.ExecuteTemplate(w, "llms.txt.tmpl", map[string]any{"Site": s.Cfg.SiteName, "SiteURL": s.Cfg.SiteURL, "Stats": st})
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	d := s.base("For AI agents", "How agents find, verify and install MCP servers from "+s.Cfg.SiteName+".")
	d.Nav = "agents"
	s.render(w, "agents", d)
}

func (s *Server) submitForm(w http.ResponseWriter, r *http.Request) {
	d := s.base("Add your MCP server", "Register a GitHub repo, npm/PyPI package or remote endpoint.")
	d.Nav = "submit"
	s.render(w, "submit", d)
}

// clientKey identifies a submitter for rate limiting. Behind Caddy the last
// X-Forwarded-For entry is the address Caddy saw; earlier entries are
// client-supplied and can be forged, so they are ignored.
func clientKey(r *http.Request) string {
	ip := r.Header.Get("X-Forwarded-For")
	if i := strings.LastIndex(ip, ","); i >= 0 {
		ip = ip[i+1:]
	}
	if ip == "" {
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	sum := sha256.Sum256([]byte("mcpkg-submit:" + strings.TrimSpace(ip)))
	return hex.EncodeToString(sum[:8])
}

func (s *Server) submitPost(w http.ResponseWriter, r *http.Request) {
	d := s.base("Add your MCP server", "")
	d.Nav = "submit"
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		d.Error = "Could not read the form."
		s.render(w, "submit", d)
		return
	}
	link := strings.TrimSpace(r.PostFormValue("url"))
	d.Form = link
	key := clientKey(r)
	if n, err := s.Store.RecentSubmissions(r.Context(), key); err == nil && n >= submitPerClientHour {
		d.Error, d.Status = "Too many submissions from your network in the last hour. Please try again later.", http.StatusTooManyRequests
		s.render(w, "submit", d)
		return
	}
	if n, err := s.Store.SubmissionsLastHour(r.Context()); err == nil && n >= submitGlobalHour {
		if s.Log != nil {
			s.Log.Warn("global submission cap reached", "last_hour", n)
		}
		d.Error, d.Status = "We are receiving a lot of submissions right now. Please try again in an hour.", http.StatusTooManyRequests
		s.render(w, "submit", d)
		return
	}
	if len(link) > 500 {
		d.Error = "That link is too long."
		s.render(w, "submit", d)
		return
	}
	if s.Resolver == nil || link == "" {
		d.Error = "Paste a link first."
		s.render(w, "submit", d)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, _, err := s.Resolver.Submit(ctx, link)
	if err != nil {
		_ = s.Store.RecordSubmission(r.Context(), link, key, 0, "rejected", err.Error())
		d.Error = err.Error()
		s.render(w, "submit", d)
		return
	}
	_ = s.Store.RecordSubmission(r.Context(), link, key, res.ToolID, "accepted", "")
	t, err := s.Store.GetTool(r.Context(), res.ToolID)
	if err == nil {
		d.Message = fmt.Sprintf("Thanks! %s is queued for checks (health → security → smoke test → page). Its page will be at %s/tool/%s within a few minutes.", t.Name, s.Cfg.SiteURL, t.Slug)
	} else {
		d.Message = "Thanks! Your server is queued for checks."
	}
	d.Form = ""
	s.render(w, "submit", d)
}

func (s *Server) adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Cfg.AdminPassword == "" {
			http.Error(w, "admin disabled (set ADMIN_PASSWORD)", http.StatusForbidden)
			return
		}
		// CSRF guard for state-changing admin requests: same-origin only.
		if r.Method == http.MethodPost {
			origin := r.Header.Get("Origin")
			if origin == "" {
				origin = r.Header.Get("Referer")
			}
			if origin != "" && !strings.Contains(origin, "://"+r.Host) {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(s.Cfg.AdminPassword)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="mcpkg admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *Server) review(w http.ResponseWriter, r *http.Request) {
	d := s.base("Review queue", "")
	var err error
	if d.Reviews, err = s.Store.OpenReviews(r.Context(), 200); err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, "review", d)
}

func (s *Server) reviewPost(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	state := r.FormValue("state")
	if state != "approved" && state != "rejected" && state != "hidden" {
		http.Error(w, "bad state", 400)
		return
	}
	if err := s.Store.ResolveReview(r.Context(), id, state, "admin"); err != nil {
		s.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/review", http.StatusSeeOther)
}
