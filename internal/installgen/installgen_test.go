package installgen

import (
	"strings"
	"testing"
	"time"

	"github.com/mcpkg/mcpkg/internal/model"
)

func client(m model.InstallMethod, name string) string {
	for _, c := range m.Clients {
		if c.Client == name {
			return c.Content
		}
	}
	return ""
}

func TestNpmWithSecret(t *testing.T) {
	tl := &model.Tool{Name: "io.github.foo/youtube", Slug: "io.github.foo.youtube", Version: "1.2.0", SmokeStatus: "works",
		Packages: []model.Package{{RegistryType: "npm", Identifier: "@foo/youtube-mcp", Version: "1.2.0", Transport: model.Transport{Type: "stdio"},
			EnvironmentVariables: []model.EnvVar{
				{Name: "YOUTUBE_API_KEY", IsRequired: true, IsSecret: true, Description: "YouTube Data API key"},
				{Name: "LOG_LEVEL", Description: "optional"},
			}}},
		Smoke: &model.Smoke{Status: "works", Method: "npm", Tools: []model.ToolInfo{{Name: "get_transcript"}}},
	}
	m := Build(tl, "https://mcpkg.dev", time.Now())
	if len(m.Methods) != 1 {
		t.Fatalf("methods = %d", len(m.Methods))
	}
	cc := client(m.Methods[0], "claude-code")
	want := "claude mcp add youtube -e 'YOUTUBE_API_KEY=<YOUTUBE_API_KEY>' -- npx -y @foo/youtube-mcp@1.2.0"
	if cc != want {
		t.Errorf("claude code:\n got %s\nwant %s", cc, want)
	}
	desktop := client(m.Methods[0], "claude-desktop")
	if strings.Contains(desktop, `\`+"u003c") || !strings.Contains(desktop, `"YOUTUBE_API_KEY": "<YOUTUBE_API_KEY>"`) {
		t.Errorf("desktop JSON must keep placeholders readable:\n%s", desktop)
	}
	if strings.Contains(desktop, "LOG_LEVEL") {
		t.Error("optional env vars must not be written into configs")
	}
	vs := client(m.Methods[0], "vscode")
	if !strings.Contains(vs, `${input:youtube-api-key}`) || !strings.Contains(vs, `"password": true`) {
		t.Errorf("vscode should prompt for secrets:\n%s", vs)
	}
	codex := client(m.Methods[0], "codex")
	if !strings.Contains(codex, "[mcp_servers.youtube]") || !strings.Contains(codex, `args = ["-y", "@foo/youtube-mcp@1.2.0"]`) {
		t.Errorf("codex toml:\n%s", codex)
	}
	if len(m.Env) != 2 || !m.Env[0].Required || m.Env[1].Required {
		t.Errorf("env list: %+v", m.Env)
	}
	if len(m.Expected) != 1 || m.Expected[0] != "get_transcript" {
		t.Errorf("expected tools: %v", m.Expected)
	}
	if !strings.Contains(strings.Join(m.AgentSteps, " "), "Never invent") {
		t.Error("agent steps must tell agents not to invent secrets")
	}
}

func TestPyPIEntryPointDiffers(t *testing.T) {
	tl := &model.Tool{Name: "mcp-server-time", Slug: "mcp-server-time",
		Packages: []model.Package{{RegistryType: "pypi", Identifier: "mcp-server-time", Version: "0.6.2", Bin: "mcp-server-time"}}}
	m := Build(tl, "https://x", time.Now())
	if got := strings.Join(m.Methods[0].Args, " "); got != "mcp-server-time==0.6.2" {
		t.Errorf("same-name entry point should use plain uvx: %q", got)
	}
	tl.Packages[0].Bin = "time-server"
	m = Build(tl, "https://x", time.Now())
	if got := strings.Join(m.Methods[0].Args, " "); got != "--from mcp-server-time==0.6.2 time-server" {
		t.Errorf("different entry point should use --from: %q", got)
	}
}

func TestNpmBinDiffers(t *testing.T) {
	tl := &model.Tool{Name: "@x/pkg", Packages: []model.Package{{RegistryType: "npm", Identifier: "@x/pkg", Version: "1.0.0", Bin: "pkg-mcp"}}}
	m := Build(tl, "https://x", time.Now())
	if got := strings.Join(m.Methods[0].Args, " "); got != "-y --package=@x/pkg@1.0.0 pkg-mcp" {
		t.Errorf("npx with explicit bin: %q", got)
	}
}

func TestRemote(t *testing.T) {
	tl := &model.Tool{Name: "com.example/api", Remotes: []model.Remote{{Type: "streamable-http", URL: "https://api.example.com/mcp",
		Headers: []model.KeyValue{{Name: "Authorization", Value: "Bearer {token}", IsSecret: true}}}}}
	m := Build(tl, "https://x", time.Now())
	cc := client(m.Methods[0], "claude-code")
	want := "claude mcp add --transport http api https://api.example.com/mcp --header 'Authorization: <Authorization>'"
	if cc != want {
		t.Errorf("remote claude code:\n got %s\nwant %s", cc, want)
	}
	if !strings.Contains(client(m.Methods[0], "claude-desktop"), "mcp-remote") {
		t.Error("desktop should bridge remote servers via mcp-remote")
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"abc": "abc", "a b": "'a b'", "it's": `'it'\''s'`, "": "''", "K=<V>": "'K=<V>'"} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q)=%s want %s", in, got, want)
		}
	}
}
