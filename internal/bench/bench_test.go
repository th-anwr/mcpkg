package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mcpkg/mcpkg/internal/httpx"
	"github.com/mcpkg/mcpkg/internal/model"
)

func TestRepoSlug(t *testing.T) {
	cases := map[string]string{
		"https://github.com/owner/name":     "owner/name",
		"https://github.com/owner/name/":    "owner/name",
		"https://github.com/owner/name.git": "owner/name",
		"http://github.com/a/b":             "a/b",
		"https://gitlab.com/a/b":            "",
		"https://github.com/onlyowner":      "",
		"https://github.com/o/n/sub":        "o/n",
		"git@github.com:owner/name.git":     "",
	}
	for in, want := range cases {
		if got := repoSlug(in); got != want {
			t.Errorf("repoSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResultValid(t *testing.T) {
	now := time.Now().UTC()
	base := Result{Scanner: "scorecard", ScannerVersion: "v1", RanAt: now, Verdict: VerdictPass}
	if err := base.Valid(); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	bad := []Result{
		{Scanner: "", ScannerVersion: "v1", RanAt: now, Verdict: VerdictPass},
		{Scanner: "x", ScannerVersion: "", RanAt: now, Verdict: VerdictPass},
		{Scanner: "x", ScannerVersion: "v1", Verdict: VerdictPass},
		{Scanner: "x", ScannerVersion: "v1", RanAt: now, Verdict: "banana"},
		{Scanner: "x", ScannerVersion: "v1", RanAt: now, Verdict: VerdictSkip},                        // skip without reason
		{Scanner: "x", ScannerVersion: "v1", RanAt: now, Verdict: VerdictPass, SkipReason: "no-repo"}, // reason on pass
	}
	for i, r := range bad {
		if err := r.Valid(); err == nil {
			t.Errorf("bad result %d accepted", i)
		}
	}
	big := base
	big.Raw = json.RawMessage(make([]byte, MaxRawBytes+1))
	if err := big.Valid(); err == nil {
		t.Error("oversized raw accepted")
	}
}

func TestUnknownScannerIgnored(t *testing.T) {
	r := &Runner{}
	if got := len(r.Scanners([]string{"scorecard", "nonsense", "osv"})); got != 2 {
		t.Fatalf("Scanners() = %d entries, want 2", got)
	}
}

func TestRunToolPanicBecomesError(t *testing.T) {
	panicScanner := &fakeScanner{name: "boom", version: "v1", panic: true}
	r := &Runner{}
	rows := r.RunTool(t.Context(), []Scanner{panicScanner}, Target{Slug: "x"})
	if len(rows) != 1 || rows[0].Verdict != VerdictError {
		t.Fatalf("panic scanner: %+v", rows)
	}
}

func TestHandshakeSSRFGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		if id, ok := m["id"]; !ok {
			w.WriteHeader(202)
			return
		} else {
			result := `{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"x","version":"1"}}`
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%v,"result":%s}`, id, result)
		}
	}))
	defer srv.Close()
	target := Target{Slug: "x", Remotes: []model.Remote{{Type: "streamable-http", URL: srv.URL}}}

	// Default: loopback refused (the production SSRF guard).
	c := &Handshake{}
	if r := c.Run(t.Context(), target); r.Verdict != VerdictFail {
		t.Fatalf("private endpoint without AllowPrivate: verdict=%s, want fail", r.Verdict)
	}
	// Tests only: allowed.
	c.AllowPrivate = true
	if r := c.Run(t.Context(), target); r.Verdict != VerdictPass {
		t.Fatalf("AllowPrivate: verdict=%s, want pass (%s)", r.Verdict, r.Raw)
	}
}

type fakeScanner struct {
	name, version string
	panic         bool
}

func (f *fakeScanner) Name() string    { return f.name }
func (f *fakeScanner) Version() string { return f.version }
func (f *fakeScanner) Run(_ context.Context, _ Target) Result {
	if f.panic {
		panic("boom")
	}
	return Result{Scanner: f.name, ScannerVersion: f.version, RanAt: time.Now().UTC(), Verdict: VerdictPass}
}

// TestSkipSemantics: "not measured" must never read as "failed" on the page.
func TestSkipSemantics(t *testing.T) {
	sc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "unknown"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "broken"):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			fmt.Fprint(w, `{"score":7.5,"date":"2026-11-01"}`)
		}
	}))
	defer sc.Close()
	hc := httpx.New("bench-test/1")
	hc.Retries = 0
	card := &Scorecard{API: sc.URL, HTTP: hc}
	cases := []struct {
		repo   string
		want   Verdict
		reason string
	}{
		{"https://github.com/acme/ok", VerdictPass, ""},
		{"https://github.com/acme/unknown", VerdictSkip, SkipNoData}, // not in the Scorecard dataset
		{"https://github.com/acme/broken", VerdictError, ""},         // upstream outage is not a tool failure
		{"https://gitlab.com/acme/x", VerdictSkip, SkipNoRepo},
	}
	for _, c := range cases {
		r := card.Run(t.Context(), Target{Slug: "x", RepoURL: c.repo})
		if r.Verdict != c.want || r.SkipReason != c.reason {
			t.Errorf("scorecard %s: %s/%s, want %s/%s", c.repo, r.Verdict, r.SkipReason, c.want, c.reason)
		}
		if err := r.Valid(); err != nil {
			t.Errorf("scorecard %s: %v", c.repo, err)
		}
	}

	osv := &OSV{API: "http://127.0.0.1:1", HTTP: hc} // never called: no supported ecosystem
	r := osv.Run(t.Context(), Target{Slug: "x", Packages: []model.Package{{RegistryType: "oci", Identifier: "ghcr.io/a/b"}}})
	if r.Verdict != VerdictSkip || r.SkipReason != SkipNotApplicable {
		t.Errorf("osv with only OCI packages: %s/%s, want skip/not-applicable", r.Verdict, r.SkipReason)
	}

	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer auth.Close()
	h := &Handshake{AllowPrivate: true}
	if r := h.Run(t.Context(), Target{Slug: "x", Remotes: []model.Remote{{Type: "streamable-http", URL: auth.URL}}}); r.Verdict != VerdictSkip || r.SkipReason != SkipAuthRequired {
		t.Errorf("401 endpoint: %s/%s, want skip/auth-required", r.Verdict, r.SkipReason)
	}
	if r := h.Run(t.Context(), Target{Slug: "x", Remotes: []model.Remote{{Type: "streamable-http", URL: "https://{tenant}.example.com/mcp"}}}); r.Verdict != VerdictSkip || r.SkipReason != SkipNeedsConfig {
		t.Errorf("templated URL: %s/%s, want skip/needs-config", r.Verdict, r.SkipReason)
	}
}

func TestRawJSONStaysValid(t *testing.T) {
	big := strings.Repeat("x", MaxRawBytes+10)
	raw := rawJSON(map[string]any{"out": big})
	if !json.Valid(raw) || len(raw) > MaxRawBytes {
		t.Fatalf("oversized raw output must become a small valid marker, got %d bytes valid=%v", len(raw), json.Valid(raw))
	}
}
