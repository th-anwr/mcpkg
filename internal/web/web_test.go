package web

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mcpkg/mcpkg/internal/config"
	"github.com/mcpkg/mcpkg/internal/model"
	"github.com/mcpkg/mcpkg/internal/store"
)

func TestRenderReadmeIsSafe(t *testing.T) {
	tl := &model.Tool{RepoURL: "https://github.com/o/r", RepoSubfolder: "src/x", ReadmeURL: "https://github.com/o/r/blob/HEAD/src/x/README.md",
		Readme: "# Title\n\n<script>alert(1)</script>\n\n[docs](docs/usage.md) [bad](javascript:alert(1)) [ext](https://example.com)\n\n![logo](img/logo.png) ![plain](http://tracker.example/p.gif)\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"}
	html := string(RenderReadme(tl))
	if strings.Contains(html, "<script>") {
		t.Error("raw HTML must not render")
	}
	if strings.Contains(html, "javascript:") {
		t.Error("javascript: links must be dropped")
	}
	if !strings.Contains(html, `href="https://github.com/o/r/blob/HEAD/src/x/docs/usage.md"`) {
		t.Errorf("relative link not rewritten:\n%s", html)
	}
	if !strings.Contains(html, `src="https://raw.githubusercontent.com/o/r/HEAD/src/x/img/logo.png"`) {
		t.Errorf("relative image not rewritten:\n%s", html)
	}
	if strings.Contains(html, "tracker.example") {
		t.Error("plain-http images must be dropped")
	}
	if !strings.Contains(html, `rel="nofollow ugc noopener"`) || !strings.Contains(html, "<table>") {
		t.Errorf("external link rel / GFM table missing:\n%s", html)
	}
}

func TestBadge(t *testing.T) {
	svg := BadgeSVG("mcpkg", "A 91 · works", "#2e7d32")
	if !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, "A 91 · works") || !strings.Contains(svg, `fill="#2e7d32"`) {
		t.Errorf("badge:\n%s", svg)
	}
	if strings.Contains(BadgeSVG("<x>", "y", "#000"), "<x>") {
		t.Error("badge text must be escaped")
	}
}

func TestSchemaParams(t *testing.T) {
	ps := schemaParams([]byte(`{"type":"object","properties":{"b":{"type":"string"},"a":{"type":["number","null"],"description":"A"}},"required":["b"]}`))
	if len(ps) != 2 || ps[0].Name != "b" || !ps[0].Required || ps[1].Type != "number | null" {
		t.Errorf("params = %+v", ps)
	}
}

// TestSecurityHeaders guards the CSP against string-literal mistakes: the
// image allowlist must be concatenated in, not quoted as source text.
func TestSecurityHeaders(t *testing.T) {
	s := &Server{}
	h := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"img-src 'self' data: https://raw.githubusercontent.com", "https://img.shields.io", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q:\n%s", want, csp)
		}
	}
	if strings.ContainsAny(csp, "`+") {
		t.Errorf("CSP contains Go source text: %s", csp)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("X-Frame-Options missing")
	}
}

// TestBenchmarksTab: raw output renders as text, the tab is reachable from
// other tabs when rows exist, and /api/bench rows encode even with no raw output.
func TestBenchmarksTab(t *testing.T) {
	s, err := New(config.Config{SiteName: "mcpkg", SiteURL: "http://x"}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	rows := []store.BenchmarkRow{
		{Scanner: "osv", ScannerVersion: "api-v1", RanAt: time.Now(), Verdict: "pass", Raw: json.RawMessage(`{"malicious":[]}`)},
		{Scanner: "scorecard", ScannerVersion: "api-v1", RanAt: time.Now(), Verdict: "skip", SkipReason: "no-data"},
	}
	for _, tab := range []string{"readme", "benchmarks"} {
		d := s.base("t", "")
		d.Tool, d.Tab, d.Benchmarks = &model.Tool{Slug: "a", Name: "a"}, tab, rows
		var buf bytes.Buffer
		if err := s.pages["tool"].ExecuteTemplate(&buf, "layout.html", d); err != nil {
			t.Fatal(err)
		}
		h := buf.String()
		if !strings.Contains(h, "?tab=benchmarks") {
			t.Errorf("tab=%s: Benchmarks tab link missing", tab)
		}
		if tab == "benchmarks" && (!strings.Contains(h, "&#34;malicious&#34;") || strings.Contains(h, "[123 34")) {
			t.Errorf("raw output not rendered as text")
		}
	}
	var out bytes.Buffer
	if err := json.NewEncoder(&out).Encode(map[string]any{"benchmarks": rows}); err != nil {
		t.Fatalf("api rows do not encode: %v", err)
	}
	if !strings.Contains(out.String(), `"skip_reason": "no-data"`) && !strings.Contains(out.String(), `"skip_reason":"no-data"`) {
		t.Errorf("api shape: %s", out.String())
	}
}

// TestAdminCSRF: admin POSTs must come from exactly this host (ISSUES-NEW N1).
func TestAdminCSRF(t *testing.T) {
	s := &Server{Cfg: config.Config{AdminPassword: "pw"}}
	h := s.adminOnly(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	cases := []struct {
		name, origin, referer string
		want                  int
	}{
		{"same origin", "https://mcpkg.dev", "", http.StatusNoContent},
		{"same origin, referer only", "", "https://mcpkg.dev/admin/review", http.StatusNoContent},
		{"suffix attacker", "https://mcpkg.dev.attacker.net", "", http.StatusForbidden},
		{"other site", "https://attacker.net", "", http.StatusForbidden},
		{"no origin or referer", "", "", http.StatusForbidden},
		{"null origin, attacker referer", "null", "https://attacker.net/x", http.StatusForbidden},
		{"other port", "https://mcpkg.dev:8443", "", http.StatusForbidden},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "https://mcpkg.dev/admin/review", nil)
		req.SetBasicAuth("admin", "pw")
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if c.referer != "" {
			req.Header.Set("Referer", c.referer)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
	}
	// GET pages stay reachable without Origin (plain navigation).
	req := httptest.NewRequest(http.MethodGet, "https://mcpkg.dev/admin/review", nil)
	req.SetBasicAuth("admin", "pw")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("GET: status %d", rec.Code)
	}
}
