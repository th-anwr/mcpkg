package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

func TestConformanceSSRFGuard(t *testing.T) {
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
	c := &Conformance{}
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
