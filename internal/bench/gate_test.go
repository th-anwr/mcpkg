package bench_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mcpkg/mcpkg/internal/bench"
	"github.com/mcpkg/mcpkg/internal/db"
	"github.com/mcpkg/mcpkg/internal/httpx"
	"github.com/mcpkg/mcpkg/internal/model"
	"github.com/mcpkg/mcpkg/internal/stages"
	"github.com/mcpkg/mcpkg/internal/store"
)

// TestGate runs the tier-1 gates (B0, B1, B2, B3, B5) against 12 known-answer
// fixture tools with fake upstreams, then writes bench/gate.json. Any red gate
// fails the test, which fails the bench-gate CI job, which blocks deploy.
func TestGate(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the bench gate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	st := store.New(conn)

	// --- Fake upstreams -----------------------------------------------------
	scorecard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/projects/github.com/")
		if strings.Contains(name, "dead") {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":"not found"}`)
			return
		}
		score := 9.0
		if strings.Contains(name, "warn") {
			score = 4.0
		}
		fmt.Fprintf(w, `{"score":%v,"date":"2026-11-01"}`, score)
	}))
	defer scorecard.Close()
	osvAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Package struct {
				Name string `json:"name"`
			} `json:"package"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case strings.HasPrefix(body.Package.Name, "mal"):
			fmt.Fprint(w, `{"vulns":[{"id":"MAL-2026-9999"}]}`)
		case strings.HasPrefix(body.Package.Name, "warn"):
			fmt.Fprint(w, `{"vulns":[{"id":"GHSA-xxxx-xxxx-xxxx"}]}`)
		default:
			fmt.Fprint(w, `{"vulns":[]}`)
		}
	}))
	defer osvAPI.Close()
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "dead") {
			w.WriteHeader(500)
			return
		}
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		if id, ok := m["id"]; !ok {
			w.WriteHeader(202)
			return
		} else {
			result := `{}`
			switch m["method"] {
			case "initialize":
				result = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"fixture","version":"1.0.0"}}`
			case "tools/list":
				result = `{"tools":[{"name":"ping","description":"Ping","inputSchema":{"type":"object"}}]}`
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%v,"result":%s}`, id, result)
		}
	}))
	defer mcpSrv.Close()

	// --- Fixture tools (B2 known-answer set) --------------------------------
	type fixture struct {
		slug, repo, pkg string
		want            map[string]bench.Verdict // scanner -> verdict
	}
	mk := func(slug, repo, pkg string, want map[string]bench.Verdict) fixture {
		return fixture{slug, repo, pkg, want}
	}
	fixtures := []fixture{
		mk("clean-a", "https://github.com/acme/alpha", "alpha", map[string]bench.Verdict{"scorecard": bench.VerdictPass, "osv": bench.VerdictPass, "mcp-conformance": bench.VerdictPass}),
		mk("clean-b", "https://github.com/acme/beta", "beta", map[string]bench.Verdict{"scorecard": bench.VerdictPass, "osv": bench.VerdictPass, "mcp-conformance": bench.VerdictPass}),
		mk("clean-c", "https://github.com/acme/gamma", "gamma", map[string]bench.Verdict{"scorecard": bench.VerdictPass, "osv": bench.VerdictPass, "mcp-conformance": bench.VerdictPass}),
		mk("mal-a", "https://github.com/acme/mal-a", "mal-a", map[string]bench.Verdict{"scorecard": bench.VerdictPass, "osv": bench.VerdictFail, "mcp-conformance": bench.VerdictPass}),
		mk("mal-b", "https://github.com/acme/mal-b", "mal-b", map[string]bench.Verdict{"scorecard": bench.VerdictPass, "osv": bench.VerdictFail, "mcp-conformance": bench.VerdictPass}),
		mk("mal-c", "https://github.com/acme/mal-c", "mal-c", map[string]bench.Verdict{"scorecard": bench.VerdictPass, "osv": bench.VerdictFail, "mcp-conformance": bench.VerdictPass}),
		mk("warn-a", "https://github.com/acme/warn-a", "warn-a", map[string]bench.Verdict{"scorecard": bench.VerdictWarn, "osv": bench.VerdictWarn, "mcp-conformance": bench.VerdictPass}),
		mk("warn-b", "https://github.com/acme/warn-b", "warn-b", map[string]bench.Verdict{"scorecard": bench.VerdictWarn, "osv": bench.VerdictWarn, "mcp-conformance": bench.VerdictPass}),
		mk("warn-c", "https://github.com/acme/warn-c", "warn-c", map[string]bench.Verdict{"scorecard": bench.VerdictWarn, "osv": bench.VerdictWarn, "mcp-conformance": bench.VerdictPass}),
		mk("dead-a", "https://github.com/acme/dead-a", "", map[string]bench.Verdict{"scorecard": bench.VerdictFail, "osv": bench.VerdictSkip, "mcp-conformance": bench.VerdictFail}),
		mk("dead-b", "https://github.com/acme/dead-b", "", map[string]bench.Verdict{"scorecard": bench.VerdictFail, "osv": bench.VerdictSkip, "mcp-conformance": bench.VerdictFail}),
		mk("dead-c", "https://github.com/acme/dead-c", "", map[string]bench.Verdict{"scorecard": bench.VerdictFail, "osv": bench.VerdictSkip, "mcp-conformance": bench.VerdictFail}),
	}

	var tools []*model.Tool
	for i, f := range fixtures {
		packages := `[]`
		if f.pkg != "" {
			packages = fmt.Sprintf(`[{"RegistryType":"npm","Identifier":%q,"Version":"1.0.0","Transport":{"Type":"stdio"}}]`, f.pkg)
		}
		remote := fmt.Sprintf(`[{"Type":"streamable-http","URL":%q}]`, mcpSrv.URL+"/mcp")
		if strings.Contains(f.slug, "dead") {
			remote = fmt.Sprintf(`[{"Type":"streamable-http","URL":%q}]`, mcpSrv.URL+"/dead")
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO tools (slug, name, repo_url, packages, remotes) VALUES ($1,$2,$3,$4::jsonb,$5::jsonb)`,
			f.slug, f.slug, f.repo, packages, remote); err != nil {
			t.Fatalf("insert %s: %v", f.slug, err)
		}
		var id int64
		if err := conn.QueryRowContext(ctx, `SELECT id FROM tools WHERE slug=$1`, f.slug).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ti := &model.Tool{ID: id, Slug: f.slug, RepoURL: f.repo}
		if f.pkg != "" {
			ti.Packages = []model.Package{{RegistryType: "npm", Identifier: f.pkg, Version: "1.0.0"}}
		}
		ti.Remotes = []model.Remote{{Type: "streamable-http"}}
		if strings.Contains(f.slug, "dead") {
			ti.Remotes[0].URL = mcpSrv.URL + "/dead"
		} else {
			ti.Remotes[0].URL = mcpSrv.URL + "/mcp"
		}
		tools = append(tools, ti)
		fixtures[i].repo = f.repo
	}

	// --- Run -----------------------------------------------------------------
	runner := &bench.Runner{HTTP: httpx.New("bench-gate/1"), ScorecardAPI: scorecard.URL, OSVAPI: osvAPI.URL}
	scanners := runner.Scanners([]string{"scorecard", "osv", "mcp-conformance"})

	run := func() [][]bench.Result {
		out := make([][]bench.Result, len(tools))
		for i, t := range tools {
			out[i] = runner.RunTool(ctx, scanners, bench.Target{Slug: t.Slug, RepoURL: t.RepoURL, Packages: t.Packages, Remotes: t.Remotes})
		}
		return out
	}
	first := run()

	gates := []Gate{}
	// B0: schema + provenance.
	b0 := Gate{ID: "B0", Threshold: "100%", Value: "0 invalid"}
	for _, rows := range first {
		for _, r := range rows {
			if err := r.Valid(); err != nil {
				b0.Pass = false
				b0.Value = "invalid: " + err.Error()
			}
		}
	}
	if b0.Value == "0 invalid" {
		b0.Pass = true
		b0.Value = fmt.Sprintf("%d rows, 0 invalid", len(first)*len(scanners))
	}
	gates = append(gates, b0)

	// B1: coverage — every tool x scanner has a terminal verdict, no silent gaps.
	total, terminal, skippedWithReason := 0, 0, 0
	for _, rows := range first {
		for _, r := range rows {
			total++
			if r.Verdict.Valid() && r.Verdict != "" {
				terminal++
			}
			if r.Verdict == bench.VerdictSkip && r.SkipReason != "" {
				skippedWithReason++
			}
		}
	}
	b1 := Gate{ID: "B1", Threshold: "100% terminal verdicts; every skip has a reason code", Pass: total == terminal, Value: fmt.Sprintf("%d/%d terminal, %d skips with reason", terminal, total, skippedWithReason)}
	gates = append(gates, b1)

	// B2: known-answer set, 12/12 tools must land their expected verdicts.
	b2 := Gate{ID: "B2", Threshold: "12/12 tools x 3 scanners exact"}
	checked, mismatch := 0, 0
	for i, rows := range first {
		want := fixtures[i].want
		for _, r := range rows {
			exp, ok := want[r.Scanner]
			if !ok {
				continue
			}
			checked++
			if r.Verdict != exp {
				mismatch++
				b2.Value += fmt.Sprintf(" %s/%s=%s(want %s);", fixtures[i].slug, r.Scanner, r.Verdict, exp)
			}
		}
	}
	b2.Pass = mismatch == 0
	if b2.Pass {
		b2.Value = fmt.Sprintf("%d/%d exact verdicts", checked, checked)
	}
	gates = append(gates, b2)

	// B3: determinism — a second run produces identical verdicts and payloads.
	second := run()
	b3 := Gate{ID: "B3", Threshold: "0 diffs between two consecutive runs"}
	diffs := 0
	for i := range first {
		for j := range first[i] {
			a, b := first[i][j], second[i][j]
			a.RanAt, b.RanAt = time.Time{}, time.Time{}
			a.DurationMs, b.DurationMs = 0, 0
			aj, _ := json.Marshal(a)
			bj, _ := json.Marshal(b)
			if string(aj) != string(bj) {
				diffs++
				b3.Pass = false
			}
		}
	}
	if diffs == 0 {
		b3.Pass = true
	}
	b3.Value = fmt.Sprintf("%d diffs", diffs)
	gates = append(gates, b3)

	// B5 (partial): the stage never changes score/status/visibility/install,
	// always returns Continue, and stores exactly one row per scanner per tool
	// (re-running replaces, never duplicates).
	deps := &stages.Deps{Store: st, Log: slogDiscard()}
	bstage := &stages.Bench{D: deps, Runner: runner, Scanners: scanners, Enabled: true}
	sres := bstage.Run(ctx, tools)
	b5 := Gate{ID: "B5", Threshold: "Continue only, no fields, no review; 36 rows after two saves"}
	stageOK := true
	for _, r := range sres {
		if r.Decision != "continue" || len(r.Fields) != 0 || r.Review != "" || r.Err != nil {
			stageOK = false
			b5.Value = fmt.Sprintf("bad stage result: %+v", r)
		}
	}
	// Persist twice; second save must replace, not duplicate.
	for i := range tools {
		rows := resultsToRows(first[i])
		if err := st.SaveBenchmarks(ctx, tools[i].ID, 0, rows); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveBenchmarks(ctx, tools[i].ID, 0, rows); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM tool_benchmarks WHERE run_id IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	wantRows := len(tools) * len(scanners)
	b5.Pass = stageOK && n == wantRows
	if stageOK && n == wantRows {
		b5.Value = fmt.Sprintf("%d rows after two saves", n)
	} else if stageOK {
		b5.Value = fmt.Sprintf("rows after two saves: %d, want %d", n, wantRows)
	}
	gates = append(gates, b5)

	// --- Gate report ----------------------------------------------------------
	report := map[string]any{
		"sha":     orEnv("GATE_SHA", "local"),
		"tier":    1,
		"at":      time.Now().UTC().Format(time.RFC3339),
		"fixture": "12 known-answer tools, fake scorecard/osv/mcp upstreams",
		"gates":   gates,
	}
	b, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile("bench/gate.json", b, 0o644); err != nil {
		t.Fatalf("write gate report: %v", err)
	}
	red := 0
	for _, g := range gates {
		if !g.Pass {
			red++
			t.Errorf("gate %s RED: %s", g.ID, g.Value)
		}
	}
	if red > 0 {
		t.Fatalf("%d/%d gates red: deploy is blocked (docs/BENCH_GATE.md)", red, len(gates))
	}
}

// Gate is one line of the gate report.
type Gate struct {
	ID        string `json:"id"`
	Pass      bool   `json:"pass"`
	Value     string `json:"value"`
	Threshold string `json:"threshold"`
}

func orEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func resultsToRows(rs []bench.Result) []store.BenchmarkRow {
	out := make([]store.BenchmarkRow, 0, len(rs))
	for _, r := range rs {
		out = append(out, store.BenchmarkRow{
			Scanner: r.Scanner, ScannerVersion: r.ScannerVersion, ImageDigest: r.ImageDigest, RanAt: r.RanAt,
			DurationMs: r.DurationMs, ExitCode: r.ExitCode, Verdict: string(r.Verdict),
			SkipReason: r.SkipReason, Raw: r.Raw,
		})
	}
	return out
}
