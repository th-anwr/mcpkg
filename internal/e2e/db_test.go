package e2e

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mcpkg/mcpkg/internal/config"
	"github.com/mcpkg/mcpkg/internal/web"

	"github.com/mcpkg/mcpkg/internal/discover"
	"github.com/mcpkg/mcpkg/internal/model"
	"github.com/mcpkg/mcpkg/internal/queue"
	"github.com/mcpkg/mcpkg/internal/sources"
	"github.com/mcpkg/mcpkg/internal/store"
)

func TestQueueSemantics(t *testing.T) {
	conn := testDB(t)
	ctx := context.Background()
	q := queue.New(conn)
	var a, b int64
	_ = conn.QueryRow(`INSERT INTO tools (slug, name) VALUES ('a','a') RETURNING id`).Scan(&a)
	_ = conn.QueryRow(`INSERT INTO tools (slug, name) VALUES ('b','b') RETURNING id`).Scan(&b)

	if ok, err := q.Enqueue(ctx, "health", a, time.Time{}); !ok || err != nil {
		t.Fatalf("enqueue: %v %v", ok, err)
	}
	if ok, _ := q.Enqueue(ctx, "health", a, time.Time{}); ok {
		t.Fatal("duplicate active job must be ignored")
	}
	if n, err := q.EnqueueMany(ctx, "health", []int64{a, b}, time.Time{}); err != nil || n != 1 {
		t.Fatalf("enqueue many: n=%d err=%v", n, err)
	}
	jobs, err := q.Claim(ctx, "health", 10, "w1", time.Minute)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("claim: %d %v", len(jobs), err)
	}
	if again, _ := q.Claim(ctx, "health", 10, "w2", time.Minute); len(again) != 0 {
		t.Fatal("claimed jobs must not be claimable again")
	}
	// Failure with retries left -> requeued in the future.
	if err := q.Fail(ctx, jobs[0], errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if due, _ := q.Claim(ctx, "health", 10, "w3", time.Minute); len(due) != 0 {
		t.Fatal("failed job should back off before retrying")
	}
	// Expired lease comes back.
	if _, err := conn.Exec(`UPDATE jobs SET locked_until = now() - interval '1 minute' WHERE id=$1`, jobs[1].ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := q.ReapExpired(ctx); n != 1 {
		t.Fatalf("reap = %d", n)
	}
	re, _ := q.Claim(ctx, "health", 10, "w4", time.Minute)
	if len(re) != 1 || re[0].Attempts != 2 {
		t.Fatalf("reaped job should be claimable with attempt 2: %+v", re)
	}
	// Exhausted attempts -> dead.
	re[0].Attempts = re[0].MaxAttempts
	_ = q.Fail(ctx, re[0], errors.New("final"))
	var state string
	_ = conn.QueryRow(`SELECT state FROM jobs WHERE id=$1`, re[0].ID).Scan(&state)
	if state != "dead" {
		t.Fatalf("state = %s", state)
	}
	if ok, _ := q.Enqueue(ctx, "health", re[0].ToolID, time.Time{}); !ok {
		t.Fatal("dead jobs must not block new ones")
	}
}

func TestDiscoverDedupe(t *testing.T) {
	conn := testDB(t)
	ctx := context.Background()
	st := store.New(conn)
	d := &discover.Discoverer{Store: st, Queue: queue.New(conn), EntryStage: "health", Log: quietLog()}

	// npm sees the package first...
	npmC := sources.Candidate{Source: "npm", Name: "@acme/weather-mcp", Version: "1.0.0", Description: "Weather from npm",
		RepoURL: "git+https://github.com/acme/mono.git", RepoSubfolder: "packages/weather",
		Packages: []model.Package{{RegistryType: "npm", Identifier: "@acme/weather-mcp", Version: "1.0.0", Transport: model.Transport{Type: "stdio"}}}}
	r1, err := d.Upsert(ctx, npmC)
	if err != nil || !r1.Created || !r1.Enqueued {
		t.Fatalf("first upsert: %+v %v", r1, err)
	}
	// ...then the official registry publishes it with env vars: same tool, richer data.
	regC := sources.Candidate{Source: "registry", Name: "io.github.acme/weather", Title: "Acme Weather", Version: "1.0.0", Status: "active",
		RepoURL: "https://github.com/acme/mono", RepoSubfolder: "packages/weather",
		Packages: []model.Package{{RegistryType: "npm", Identifier: "@acme/weather-mcp", Version: "1.0.0", Transport: model.Transport{Type: "stdio"},
			EnvironmentVariables: []model.EnvVar{{Name: "WEATHER_KEY", IsRequired: true, IsSecret: true}}}}}
	r2, err := d.Upsert(ctx, regC)
	if err != nil || r2.Created || r2.ToolID != r1.ToolID {
		t.Fatalf("registry should merge into the npm tool: %+v %v", r2, err)
	}
	tl, _ := st.GetTool(ctx, r1.ToolID)
	if tl.Name != "io.github.acme/weather" || tl.Title != "Acme Weather" || tl.Description != "Weather from npm" {
		t.Errorf("composed fields: name=%q title=%q desc=%q", tl.Name, tl.Title, tl.Description)
	}
	if len(tl.Packages) != 1 || len(tl.Packages[0].EnvironmentVariables) != 1 {
		t.Errorf("registry package should win: %+v", tl.Packages)
	}
	if tl.Slug != "acme.weather-mcp" {
		t.Errorf("slug must stay stable: %s", tl.Slug)
	}
	// A different package from the same monorepo is a different tool.
	other := sources.Candidate{Source: "npm", Name: "@acme/maps-mcp", Version: "2.0.0", RepoURL: "https://github.com/acme/mono",
		Packages: []model.Package{{RegistryType: "npm", Identifier: "@acme/maps-mcp", Version: "2.0.0"}}}
	r3, _ := d.Upsert(ctx, other)
	if r3.ToolID == r1.ToolID || !r3.Created {
		t.Fatalf("monorepo sibling must not merge: %+v", r3)
	}
	// A GitHub-search hit for a repo we know only adds stats.
	gh := sources.Candidate{Source: "github", Name: "acme/solo", RepoURL: "https://github.com/acme/solo", Stars: 321}
	solo := sources.Candidate{Source: "pypi", Name: "acme-solo", Version: "0.1", RepoURL: "https://github.com/acme/solo",
		Packages: []model.Package{{RegistryType: "pypi", Identifier: "acme-solo", Version: "0.1"}}}
	r4, _ := d.Upsert(ctx, solo)
	r5, _ := d.Upsert(ctx, gh)
	if r5.ToolID != r4.ToolID || r5.Created {
		t.Fatalf("github hit should merge into the pypi tool: %+v vs %+v", r5, r4)
	}
	tl, _ = st.GetTool(ctx, r4.ToolID)
	if tl.Stars != 321 {
		t.Errorf("stars = %d", tl.Stars)
	}
	// Re-crawling an unchanged candidate is a no-op (no new job, no change).
	_, _ = conn.Exec(`UPDATE jobs SET state='done'`)
	r6, _ := d.Upsert(ctx, solo)
	if r6.Changed || r6.Enqueued {
		t.Errorf("unchanged re-crawl should not enqueue: %+v", r6)
	}
	// A version bump re-enqueues.
	solo.Version, solo.Packages[0].Version = "0.2", "0.2"
	r7, _ := d.Upsert(ctx, solo)
	if !r7.Changed || !r7.Enqueued {
		t.Errorf("version bump should enqueue: %+v", r7)
	}
	// Registry takedown hides immediately.
	del := regC
	del.Status = "deleted"
	_, _ = d.Upsert(ctx, del)
	tl, _ = st.GetTool(ctx, r1.ToolID)
	if tl.Status != "blocked" || tl.Visibility != "hidden" {
		t.Errorf("deleted in registry must hide: %s/%s", tl.Status, tl.Visibility)
	}
}

func TestQueuePriority(t *testing.T) {
	conn := testDB(t)
	ctx := context.Background()
	q := queue.New(conn)
	var low, high int64
	_ = conn.QueryRow(`INSERT INTO tools (slug, name) VALUES ('low','low') RETURNING id`).Scan(&low)
	_ = conn.QueryRow(`INSERT INTO tools (slug, name) VALUES ('high','high') RETURNING id`).Scan(&high)
	_, _ = q.EnqueuePriority(ctx, "smoke", low, time.Time{}, 5)
	_, _ = q.EnqueuePriority(ctx, "smoke", high, time.Time{}, 70000)
	jobs, err := q.Claim(ctx, "smoke", 1, "w", time.Minute)
	if err != nil || len(jobs) != 1 || jobs[0].ToolID != high {
		t.Fatalf("popular tool should be claimed first: %+v %v", jobs, err)
	}
}

// Pages must render for tools that were discovered but not checked yet (all JSON columns nil).
func TestPagesForUncheckedTool(t *testing.T) {
	conn := testDB(t)
	ctx := context.Background()
	st := store.New(conn)
	d := &discover.Discoverer{Store: st, Queue: queue.New(conn), EntryStage: "health", Log: quietLog()}
	_, err := d.Upsert(ctx, sources.Candidate{Source: "npm", Name: "@acme/fresh-mcp", Version: "0.1.0", Description: "Fresh <b>tool</b>",
		Packages: []model.Package{{RegistryType: "npm", Identifier: "@acme/fresh-mcp", Version: "0.1.0"}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Load()
	srv, err := web.New(cfg, st, nil, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	for _, path := range []string{"/", "/search", "/search?q=fresh&grade=B&status=works&sort=stars", "/tool/acme.fresh-mcp",
		"/tool/acme.fresh-mcp?tab=tools", "/tool/acme.fresh-mcp?tab=install", "/tool/acme.fresh-mcp?tab=health",
		"/api/tools/acme.fresh-mcp", "/api/search?q=fresh", "/v0.1/servers", "/badge/acme.fresh-mcp.svg", "/llms.txt", "/agents", "/submit"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Errorf("GET %s = %d: %s", path, rec.Code, truncateStr(rec.Body.String(), 300))
		}
		if strings.Contains(rec.Body.String(), "<b>tool</b>") {
			t.Errorf("GET %s: HTML in descriptions must be escaped", path)
		}
	}
	for path, want := range map[string]int{"/tool/acme.fresh-mcp/install.md": 404, "/api/tools/acme.fresh-mcp/manifest.json": 404, "/tool/nope": 404, "/admin/review": 403} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
	// Submissions without a resolver are refused politely, not a crash.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/submit", strings.NewReader("url=https%3A%2F%2Fgithub.com%2Fa%2Fb"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Paste a link") {
		t.Errorf("POST /submit without resolver: %d", rec.Code)
	}
}

func TestSelectStaleIDs(t *testing.T) {
	conn := testDB(t)
	ctx := context.Background()
	st := store.New(conn)
	q := queue.New(conn)
	ins := func(slug, status, visibility string, verifiedDaysAgo int) int64 {
		var id int64
		err := conn.QueryRow(`INSERT INTO tools (slug, name, status, visibility, verified_at)
			VALUES ($1,$1,$2,$3, CASE WHEN $4 < 0 THEN NULL ELSE now() - make_interval(days => $4) END) RETURNING id`,
			slug, status, visibility, verifiedDaysAgo).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	stale := ins("stale", "active", "listed", 30)
	fresh := ins("fresh", "active", "listed", 1)
	blocked := ins("blocked", "blocked", "hidden", 30)
	busy := ins("busy", "active", "listed", 30)
	never := ins("never", "new", "listed", -1)
	_, _ = q.Enqueue(ctx, "smoke", busy, time.Time{}) // mid-pipeline: must not get a second pipeline
	ids, err := st.SelectStaleIDs(ctx, 14, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if !got[stale] || !got[never] || got[fresh] || got[blocked] || got[busy] {
		t.Fatalf("stale selection wrong: %v (stale=%d never=%d fresh=%d blocked=%d busy=%d)", ids, stale, never, fresh, blocked, busy)
	}
	// After one re-enqueue, a tool that keeps failing is not picked again inside the window.
	_, _ = q.Enqueue(ctx, "health", stale, time.Time{})
	_, _ = conn.Exec(`UPDATE jobs SET state='dead' WHERE tool_id=$1`, stale)
	ids, _ = st.SelectStaleIDs(ctx, 14, 100)
	for _, id := range ids {
		if id == stale {
			t.Fatal("recently attempted tool must wait for the next window")
		}
	}
}

// TestCleanupAgesOutDeadJobs: done jobs go after the given age, dead jobs after
// queue.DeadTTL, and recent dead jobs stay for inspection (ISSUES-NEW N3).
func TestCleanupAgesOutDeadJobs(t *testing.T) {
	conn := testDB(t)
	ctx := context.Background()
	q := queue.New(conn)
	var tool int64
	if err := conn.QueryRow(`INSERT INTO tools (slug, name) VALUES ('c','c') RETURNING id`).Scan(&tool); err != nil {
		t.Fatal(err)
	}
	for _, j := range []struct {
		stage, state, age string
	}{
		{"health", "done", "8 days"},   // removed: done, older than 7 days
		{"security", "done", "1 hour"}, // kept: recent
		{"smoke", "dead", "31 days"},   // removed: dead, older than DeadTTL
		{"enrich", "dead", "2 days"},   // kept: recent dead job, still inspectable
	} {
		if _, err := conn.Exec(`INSERT INTO jobs (stage, tool_id, state, updated_at) VALUES ($1,$2,$3, now() - $4::interval)`,
			j.stage, tool, j.state, j.age); err != nil {
			t.Fatal(err)
		}
	}
	n, err := q.Cleanup(ctx, 7*24*time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("cleanup removed %d (err %v), want 2", n, err)
	}
	var left []string
	rows, err := conn.Query(`SELECT stage FROM jobs ORDER BY stage`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		left = append(left, s)
	}
	if strings.Join(left, ",") != "enrich,security" {
		t.Fatalf("jobs left: %v, want enrich,security", left)
	}
}
