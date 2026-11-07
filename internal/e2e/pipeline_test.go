package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mcpkg/mcpkg/internal/app"
	"github.com/mcpkg/mcpkg/internal/config"
	"github.com/mcpkg/mcpkg/internal/httpx"
	"github.com/mcpkg/mcpkg/internal/pipeline"
	"github.com/mcpkg/mcpkg/internal/sources"
	"github.com/mcpkg/mcpkg/internal/web"
)

// fakeUpstream emulates the APIs we cannot (or should not) hit from tests:
// MCP Registry, GitHub GraphQL, OSV, OpenSSF Scorecard, npm downloads, an
// OpenAI-compatible LLM, and a remote Streamable-HTTP MCP server.
func fakeUpstream(t *testing.T, everythingVer, timeVer string) *httptest.Server {
	mux := http.NewServeMux()
	var self string
	mux.HandleFunc("GET /v0.1/servers", func(w http.ResponseWriter, r *http.Request) {
		meta := func(status string) string {
			return fmt.Sprintf(`"_meta":{"io.modelcontextprotocol.registry/official":{"status":%q,"isLatest":true,"publishedAt":"2026-09-01T00:00:00Z","updatedAt":"2026-10-01T00:00:00Z"}}`, status)
		}
		items := []string{
			fmt.Sprintf(`{"server":{"name":"io.github.modelcontextprotocol/server-everything","title":"Everything","description":"Reference server that exercises every MCP feature","version":%q,
				"repository":{"url":"https://github.com/modelcontextprotocol/servers","source":"github","subfolder":"src/everything"},
				"packages":[{"registryType":"npm","identifier":"@modelcontextprotocol/server-everything","version":%q,"transport":{"type":"stdio"}}]},%s}`, everythingVer, everythingVer, meta("active")),
			fmt.Sprintf(`{"server":{"name":"io.github.modelcontextprotocol/time","description":"Time and timezone conversion","version":%q,
				"repository":{"url":"https://github.com/modelcontextprotocol/servers","source":"github","subfolder":"src/time"},
				"packages":[{"registryType":"pypi","identifier":"mcp-server-time","version":%q,"transport":{"type":"stdio"}}]},%s}`, timeVer, timeVer, meta("active")),
			fmt.Sprintf(`{"server":{"name":"com.example/remote-demo","description":"A hosted demo server","version":"1.0.0",
				"remotes":[{"type":"streamable-http","url":%q}]},%s}`, self+"/mcp", meta("active")),
			fmt.Sprintf(`{"server":{"name":"io.github.fake/is-number-mcp","description":"Totally legit","version":"7.0.0","repository":{"url":"https://github.com/fake/is-number-mcp","source":"github"},
				"packages":[{"registryType":"npm","identifier":"is-number","version":"7.0.0","transport":{"type":"stdio"}}]},%s}`, meta("active")),
			fmt.Sprintf(`{"server":{"name":"io.github.spam/takedown","description":"removed","version":"0.0.1","packages":[{"registryType":"npm","identifier":"spam-takedown-x","version":"0.0.1"}]},%s}`, meta("deleted")),
		}
		fmt.Fprintf(w, `{"servers":[%s],"metadata":{"count":%d}}`, strings.Join(items, ","), len(items))
	})
	alias := regexp.MustCompile(`r(\d+): repository\(owner: "([^"]+)", name: "([^"]+)"\) \{ \.\.\.F(?: readmeSub: object\(expression: "HEAD:([^"]+)"\))?`)
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Query string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		data := map[string]any{"rateLimit": map[string]any{"remaining": 4999}}
		for _, m := range alias.FindAllStringSubmatch(body.Query, -1) {
			if m[2]+"/"+m[3] != "modelcontextprotocol/servers" {
				data["r"+m[1]] = nil
				continue
			}
			repo := map[string]any{
				"nameWithOwner": "modelcontextprotocol/servers", "isArchived": false, "stargazerCount": 70000, "forkCount": 8000,
				"createdAt": "2024-11-19T00:00:00Z", "pushedAt": time.Now().Add(-48 * time.Hour).Format(time.RFC3339),
				"licenseInfo": map[string]any{"spdxId": "MIT"}, "owner": map[string]any{"__typename": "Organization", "isVerified": true},
				"defaultBranchRef": map[string]any{"target": map[string]any{"oid": "abc123", "committedDate": time.Now().Add(-48 * time.Hour).Format(time.RFC3339), "recent": map[string]any{"totalCount": 120}}},
				"latestRelease":    map[string]any{"tagName": "2026.10.1", "publishedAt": time.Now().Add(-10 * 24 * time.Hour).Format(time.RFC3339)},
				"openIssues":       map[string]any{"totalCount": 300}, "closedIssues": map[string]any{"totalCount": 900},
				"openPRs": map[string]any{"totalCount": 150}, "mergedPRs": map[string]any{"totalCount": 1800}, "closedPRs": map[string]any{"totalCount": 600},
				"oldestPR":         map[string]any{"nodes": []any{map[string]any{"createdAt": "2025-12-01T00:00:00Z"}}},
				"mentionableUsers": map[string]any{"totalCount": 400}, "repositoryTopics": map[string]any{"nodes": []any{}},
			}
			if m[4] == "src/time/README.md" { // README delivered inside the GraphQL batch
				repo["readmeSub"] = map[string]any{"text": "# Time MCP Server (from GraphQL)\n\nA Model Context Protocol server that provides time and timezone conversion capabilities for agents.\n"}
			}
			data["r"+m[1]] = repo
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	mux.HandleFunc("POST /v1/query", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Package struct{ Name string } `json:"package"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		if q.Package.Name == "is-number" {
			_, _ = w.Write([]byte(`{"vulns":[{"id":"MAL-2026-0001"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"vulns":[]}`))
	})
	mux.HandleFunc("GET /projects/github.com/modelcontextprotocol/servers", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"score":6.5}`))
	})
	mux.HandleFunc("GET /downloads/point/last-week/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"downloads":54321}`))
	})
	mux.HandleFunc("POST /chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		content := `{"summary":"Reference MCP server that exercises prompts, resources and tools for client testing.","description":"A test server that implements every MCP feature so client developers can check their integrations.","categories":["developer-tools"],"env_help":{},"caveats":[]}`
		if strings.Contains(string(body), "mcp-server-time") {
			// Hallucinated URL: must be rejected and replaced by the deterministic fallback.
			content = `{"summary":"Converts time zones, see https://hallucinated.example.com for docs.","description":"","categories":["productivity"]}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1200, "completion_tokens": 90},
		})
	})
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		id, ok := m["id"]
		if !ok {
			w.WriteHeader(202)
			return
		}
		result := `{}`
		switch m["method"] {
		case "initialize":
			result = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"remote-demo","version":"1.0.0"}}`
		case "tools/list":
			result = `{"tools":[{"name":"search_docs","description":"Search the docs","inputSchema":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%v,"result":%s}`, id, result)
	})
	srv := httptest.NewServer(mux)
	self = srv.URL
	return srv
}

func TestFullPipeline(t *testing.T) {
	if os.Getenv("E2E_NETWORK") == "" {
		t.Skip("set E2E_NETWORK=1 (needs npm, PyPI, raw.githubusercontent.com, npm and uv installed)")
	}
	conn := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	real := httpx.New("mcpkg-e2e/0.1")
	ev, err := sources.FetchNpmLatest(ctx, real, "https://registry.npmjs.org", "@modelcontextprotocol/server-everything")
	if err != nil {
		t.Fatalf("npm: %v", err)
	}
	tm, err := sources.FetchPyPI(ctx, real, "https://pypi.org", "mcp-server-time", "")
	if err != nil {
		t.Fatalf("pypi: %v", err)
	}
	up := fakeUpstream(t, ev.Version, tm.Info.Version)
	defer up.Close()

	cfg := config.Load()
	cfg.SiteURL, cfg.SiteName, cfg.MetaNamespace = "https://mcpkg.test", "mcpkg", "dev.mcpkg"
	cfg.RegistryURL, cfg.GitHubAPI, cfg.GitHubToken = up.URL, up.URL, "test-token"
	cfg.OSVAPI, cfg.ScorecardAPI, cfg.NpmAPI = up.URL, up.URL, up.URL
	cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMReviewSample = up.URL, "fake-oss-model", 0
	cfg.NpmRegistry, cfg.PyPIURL, cfg.RawGitHub = "https://registry.npmjs.org", "https://pypi.org", "https://raw.githubusercontent.com"
	cfg.Sandbox, cfg.WorkDir, cfg.WorkerID = "process", t.TempDir(), "e2e"
	pipe := pipeline.Default()
	for name, sc := range pipe.Stages {
		sc.Concurrency = 2
		pipe.Stages[name] = sc
	}
	a := app.New(cfg, quietLog(), conn, pipe)
	a.AllowPrivateRemotes = true

	// 1. Discovery from the (fake) official registry.
	srcs, _ := a.Sources([]string{"registry"})
	st, err := a.Discoverer().Run(ctx, srcs[0], 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Created != 5 || st.Enqueued != 4 {
		t.Fatalf("discovery stats: %+v (the takedown must not be enqueued)", st)
	}

	// 2. Workers drain the queue (health -> security -> smoke -> enrich -> render).
	wctx, wcancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- a.Runner().Work(wctx, []string{"health", "security", "smoke", "enrich", "render"}) }()
	deadline := time.Now().Add(10 * time.Minute)
	for {
		var pending int
		_ = conn.QueryRow(`SELECT count(*) FROM jobs WHERE state IN ('queued','running') AND run_after <= now()`).Scan(&pending)
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pipeline did not finish in time")
		}
		time.Sleep(2 * time.Second)
	}
	wcancel()
	<-done
	var dead int
	_ = conn.QueryRow(`SELECT count(*) FROM jobs WHERE state='dead'`).Scan(&dead)
	if dead > 0 {
		rows, _ := conn.Query(`SELECT stage, tool_id, last_error FROM jobs WHERE state='dead'`)
		for rows.Next() {
			var s, e string
			var id int64
			_ = rows.Scan(&s, &id, &e)
			t.Logf("dead job: %s tool=%d err=%s", s, id, e)
		}
		t.Fatalf("%d dead jobs", dead)
	}

	// 3. Check what the pipeline stored.
	check := func(slug, wantSmoke string) {
		tl, err := a.Store.GetToolBySlug(ctx, slug)
		if err != nil {
			t.Fatalf("%s: %v", slug, err)
		}
		if tl.SmokeStatus != wantSmoke {
			msg := ""
			if tl.Smoke != nil {
				msg = tl.Smoke.Error + " | " + tl.Smoke.LogTail
			}
			t.Errorf("%s smoke = %s, want %s (%s)", slug, tl.SmokeStatus, wantSmoke, msg)
		}
		if tl.Score == nil || tl.Install == nil || tl.VerifiedAt == nil {
			t.Errorf("%s not rendered: score=%v install=%v", slug, tl.Score != nil, tl.Install != nil)
		}
	}
	check("io.github.modelcontextprotocol.server-everything", "works")
	check("io.github.modelcontextprotocol.time", "works")
	check("com.example.remote-demo", "works")

	ev2, _ := a.Store.GetToolBySlug(ctx, "io.github.modelcontextprotocol.server-everything")
	if ev2.Smoke == nil || len(ev2.Smoke.Tools) < 3 {
		t.Errorf("everything should list several tools: %+v", ev2.Smoke)
	}
	if ev2.Enrich == nil || ev2.Enrich.Source != "llm" || ev2.Health == nil || ev2.Health.Stars != 70000 || ev2.Readme == "" {
		t.Errorf("everything enrich/health/readme: %+v %+v readme=%d", ev2.Enrich, ev2.Health, len(ev2.Readme))
	}
	if ev2.Security == nil || ev2.Security.Scanned != "npm-tarball" || ev2.Security.Scorecard == nil {
		t.Errorf("everything security: %+v", ev2.Security)
	}
	tm2, _ := a.Store.GetToolBySlug(ctx, "io.github.modelcontextprotocol.time")
	if tm2.Enrich == nil || tm2.Enrich.Source != "fallback" || len(tm2.Enrich.Rejected) == 0 {
		t.Errorf("time server: hallucinated LLM output must be rejected: %+v", tm2.Enrich)
	}
	if !strings.Contains(tm2.Readme, "(from GraphQL)") || !strings.HasSuffix(tm2.ReadmeURL, "/blob/HEAD/src/time/README.md") {
		t.Errorf("time README should come from the GraphQL batch: %q %s", truncateStr(tm2.Readme, 80), tm2.ReadmeURL)
	}
	if !strings.Contains(ev2.ReadmeURL, "src/everything/README.md") {
		t.Errorf("everything README should come from the raw fallback in its subfolder: %s", ev2.ReadmeURL)
	}
	if tm2.Security == nil || tm2.Security.Scanned != "pypi-dist" || len(tm2.Security.EntryPoints) == 0 {
		t.Errorf("time security: %+v", tm2.Security)
	}
	bad, _ := a.Store.GetToolBySlug(ctx, "io.github.fake.is-number-mcp")
	if bad.Status != "blocked" || bad.Visibility != "hidden" {
		t.Errorf("OSV MAL- package must be blocked: %s/%s", bad.Status, bad.Visibility)
	}

	// 4. The website and APIs render from the template + data.
	srv, err := web.New(cfg, a.Store, a.Resolver(), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	site := httptest.NewServer(srv.Handler())
	defer site.Close()
	get := func(path string, want int) string {
		resp, err := http.Get(site.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
		return string(b)
	}
	page := get("/tool/io.github.modelcontextprotocol.server-everything", 200)
	for _, want := range []string{"Everything", "Works", "claude mcp add", "install.md for AI agents"} {
		if !strings.Contains(page, want) {
			t.Errorf("tool page missing %q", want)
		}
	}
	tools := get("/tool/io.github.modelcontextprotocol.time?tab=tools", 200)
	if !strings.Contains(tools, "get_current_time") {
		t.Error("tools tab should list get_current_time from the live tools/list")
	}
	get("/tool/io.github.modelcontextprotocol.time?tab=health", 200)
	get("/tool/io.github.modelcontextprotocol.time?tab=install", 200)
	md := get("/tool/io.github.modelcontextprotocol.server-everything/install.md", 200)
	if !strings.Contains(md, "@modelcontextprotocol/server-everything@"+ev.Version) || !strings.Contains(md, "## Verify") {
		t.Errorf("install.md:\n%s", md)
	}
	if md := get("/tool/io.github.modelcontextprotocol.time/install.md", 200); !strings.Contains(md, "uvx mcp-server-time=="+tm.Info.Version) {
		t.Errorf("time install.md should use uvx:\n%s", md)
	}
	get("/tool/io.github.fake.is-number-mcp", 404)
	get("/tool/io.github.spam.takedown", 404)
	if js := get("/api/search?q=time", 200); !strings.Contains(js, "io.github.modelcontextprotocol.time") {
		t.Errorf("search api: %s", js)
	}
	if js := get("/v0.1/servers?limit=10", 200); !strings.Contains(js, `"dev.mcpkg/trust"`) || strings.Contains(js, "is-number") {
		t.Errorf("subregistry: %s", js)
	}
	if svg := get("/badge/io.github.modelcontextprotocol.server-everything.svg", 200); !strings.Contains(svg, "works") {
		t.Errorf("badge: %s", svg)
	}
	get("/", 200)
	get("/search?q=server", 200)
	get("/llms.txt", 200)
	get("/agents", 200)
	get("/api/tools/io.github.modelcontextprotocol.time/manifest.json", 200)

	// Save rendered pages for visual review when requested.
	if dir := os.Getenv("E2E_SNAPSHOT_DIR"); dir != "" {
		for name, path := range map[string]string{
			"home.html": "/", "search.html": "/search?q=server", "tool.html": "/tool/io.github.modelcontextprotocol.server-everything",
			"tool-tools.html": "/tool/io.github.modelcontextprotocol.time?tab=tools", "tool-install.html": "/tool/io.github.modelcontextprotocol.time?tab=install",
			"tool-health.html": "/tool/io.github.modelcontextprotocol.server-everything?tab=health", "install.md": "/tool/io.github.modelcontextprotocol.server-everything/install.md",
		} {
			_ = os.WriteFile(dir+"/"+name, []byte(get(path, 200)), 0o644)
		}
	}
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
