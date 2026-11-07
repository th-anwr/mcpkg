package stages

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mcpkg/mcpkg/internal/mcpclient"
	"github.com/mcpkg/mcpkg/internal/model"
)

func TestClassify(t *testing.T) {
	ok := &mcpclient.Info{Tools: []model.ToolInfo{{Name: "x"}}}
	cases := []struct {
		info *mcpclient.Info
		err  error
		logs string
		want string
	}{
		{ok, nil, "", "works"},
		{nil, mcpclient.ErrAuthRequired, "", "auth_required"},
		{nil, mcpclient.ErrClosed, "Error: GITHUB_TOKEN environment variable is required", "needs_credentials"},
		{nil, mcpclient.ErrClosed, "Error: Invalid API key provided", "needs_credentials"},
		{nil, mcpclient.ErrClosed, "SyntaxError: Unexpected token '<'", "broken"},
		{nil, errors.New("context deadline exceeded"), "", "broken"},
		{&mcpclient.Info{ToolsError: "rpc error -32000: unauthorized"}, nil, "", "needs_credentials"},
	}
	for i, c := range cases {
		if got, _ := Classify(c.info, c.err, c.logs); got != c.want {
			t.Errorf("case %d: got %s want %s", i, got, c.want)
		}
	}
}

func enrichTool() (*model.Tool, EnrichInput) {
	tl := &model.Tool{Name: "io.github.foo/yt", RepoURL: "https://github.com/foo/yt",
		Readme:   "# YT MCP\nGet transcripts from YouTube videos. Get an API key at https://console.cloud.google.com/apis/credentials\n",
		Packages: []model.Package{{RegistryType: "npm", Identifier: "yt-mcp", EnvironmentVariables: []model.EnvVar{{Name: "YT_KEY", Description: "API key"}}}},
		Smoke:    &model.Smoke{Tools: []model.ToolInfo{{Name: "get_transcript", Description: "Fetch a transcript"}}},
	}
	return tl, BuildInput(tl)
}

func TestValidateEnrichmentAcceptsGrounded(t *testing.T) {
	tl, in := enrichTool()
	out, problems := ValidateEnrichment(`{"summary":"Lets agents fetch transcripts of YouTube videos with get_transcript.","description":"Fetches transcripts.","categories":["video-audio","bogus"],"env_help":{"YT_KEY":"YouTube Data API key from https://console.cloud.google.com/apis/credentials","OTHER":"x"},"caveats":[]}`, in, tl)
	if len(problems) > 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if len(out.Categories) != 1 || out.Categories[0] != "video-audio" {
		t.Errorf("categories must be filtered to the taxonomy: %v", out.Categories)
	}
	if _, ok := out.EnvHelp["OTHER"]; ok {
		t.Error("unknown env vars must be dropped")
	}
}

func TestValidateEnrichmentRejects(t *testing.T) {
	tl, in := enrichTool()
	bad := map[string]string{
		"hallucinated url":  `{"summary":"Fetch YouTube transcripts, docs at https://evil.example.com/docs.","description":"","categories":["video-audio"]}`,
		"install command":   `{"summary":"Run npx -y yt-mcp to fetch YouTube transcripts.","description":"","categories":["video-audio"]}`,
		"unknown tool":      `{"summary":"Fetch YouTube transcripts with delete_channel and more.","description":"","categories":["video-audio"]}`,
		"no category":       `{"summary":"Fetch YouTube transcripts for agents.","description":"","categories":["nope"]}`,
		"not json":          `Sure! Here is the JSON you asked for`,
		"markdown":          `{"summary":"**Fetch** YouTube transcripts for agents.","description":"","categories":["video-audio"]}`,
		"too short summary": `{"summary":"YT","description":"","categories":["video-audio"]}`,
	}
	for name, text := range bad {
		if _, problems := ValidateEnrichment(text, in, tl); len(problems) == 0 {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestFallback(t *testing.T) {
	tl := &model.Tool{Name: "pg-mcp", Description: "Query PostgreSQL databases from your agent. Read-only by default.",
		Readme: "# pg-mcp\n\n![badge](x.svg)\n\nThis server lets agents run read-only SQL against Postgres databases safely and quickly.\n\n```bash\nnpx pg-mcp\n```\n"}
	e := Fallback(tl, BuildInput(tl))
	if e.Summary != "Query PostgreSQL databases from your agent." {
		t.Errorf("summary = %q", e.Summary)
	}
	if !strings.Contains(e.Description, "read-only SQL") || strings.Contains(e.Description, "npx") {
		t.Errorf("description = %q", e.Description)
	}
	if len(e.Categories) == 0 || e.Categories[0] != "databases" {
		t.Errorf("categories = %v", e.Categories)
	}
}

func TestPythonEntryPoints(t *testing.T) {
	dir := t.TempDir()
	di := filepath.Join(dir, "mcp_server_time-0.6.2.dist-info")
	_ = os.MkdirAll(di, 0o755)
	_ = os.WriteFile(filepath.Join(di, "entry_points.txt"), []byte("[console_scripts]\nmcp-server-time = mcp_server_time:main\n\n[other]\nx = y\n"), 0o644)
	sd := filepath.Join(dir, "sdist")
	_ = os.MkdirAll(sd, 0o755)
	_ = os.WriteFile(filepath.Join(sd, "pyproject.toml"), []byte("[project]\nname='x'\n[project.scripts]\n\"time-cli\" = \"x:main\"\n[tool.x]\ny = 1\n"), 0o644)
	got := PythonEntryPoints(dir)
	if strings.Join(got, ",") != "mcp-server-time,time-cli" {
		t.Errorf("entry points = %v", got)
	}
}

func TestBuildGraphQLQuery(t *testing.T) {
	q := BuildGraphQLQuery([]GHRequest{{Slug: "o/r", Subfolder: "src/x"}, {Slug: `evil"/x`}}, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	if !strings.Contains(q, `r0: repository(owner: "o", name: "r") { ...F readmeSub: object(expression: "HEAD:src/x/README.md")`) {
		t.Errorf("alias/readme missing:\n%s", q)
	}
	if strings.Contains(q, `r1: repository(owner: "evil\"", name: "x") { ...F readmeSub`) {
		t.Error("no subfolder README lookup without a subfolder")
	}
	if !strings.Contains(q, `owner: "evil\""`) {
		t.Error("owner must be quoted/escaped")
	}
	if !strings.Contains(q, `history(since: "2026-07-01T00:00:00Z")`) {
		t.Error("since missing")
	}
}

func TestReadmeExcerptStripsCode(t *testing.T) {
	ex := ReadmeExcerpt("# T\n<img src=x>\n```sh\nrm -rf /\n```\nHello [link](http://a)\n", 1000)
	if strings.Contains(ex, "rm -rf") || strings.Contains(ex, "<img") || !strings.Contains(ex, "Hello") {
		t.Errorf("excerpt = %q", ex)
	}
}
