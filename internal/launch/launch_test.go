package launch

import (
	"strings"
	"testing"

	"github.com/mcpkg/mcpkg/internal/model"
)

func TestChoosePrefersNpmStdio(t *testing.T) {
	tl := &model.Tool{Packages: []model.Package{
		{RegistryType: "oci", Identifier: "mcp/x"},
		{RegistryType: "pypi", Identifier: "x"},
		{RegistryType: "npm", Identifier: "x", Transport: model.Transport{Type: "stdio"}},
	}, Remotes: []model.Remote{{Type: "sse", URL: "https://x/sse"}}}
	p := Choose(tl, "/tmp/w")
	if p.Kind != "npm" || p.PackageIdx != 2 {
		t.Errorf("plan = %+v", p)
	}
}

func TestChooseRemoteAndSkips(t *testing.T) {
	tl := &model.Tool{Remotes: []model.Remote{{Type: "sse", URL: "https://a/sse"}, {Type: "streamable-http", URL: "https://a/mcp"}}}
	if p := Choose(tl, ""); p.Kind != "remote" || p.Remote.URL != "https://a/mcp" {
		t.Errorf("should prefer streamable-http: %+v", p)
	}
	tl = &model.Tool{Remotes: []model.Remote{{Type: "streamable-http", URL: "https://{tenant}.example.com/mcp"}}}
	if p := Choose(tl, ""); p.Kind != "none" {
		t.Errorf("templated URL should be skipped: %+v", p)
	}
	tl = &model.Tool{RepoURL: "https://github.com/a/b"}
	if p := Choose(tl, ""); p.Kind != "none" || !strings.Contains(p.SkipReason, "source-only") {
		t.Errorf("source-only: %+v", p)
	}
}

func TestBuildArgsAndEnv(t *testing.T) {
	args := BuildArgs([]model.Argument{
		{Type: "positional", ValueHint: "allowed_directory", IsRequired: true},
		{Type: "named", Name: "--port", Default: "8080"},
		{Type: "named", Name: "--token", Value: "{api_token}"},
		{Type: "named", Name: "--optional"},
	}, "/tmp/work")
	if got := strings.Join(args, " "); got != "/tmp/work --port 8080 --token "+Placeholder {
		t.Errorf("args = %q", got)
	}
	env := BuildEnv([]model.EnvVar{{Name: "API_KEY", IsRequired: true}, {Name: "OPT"}, {Name: "MODE", Default: "fast"}, {Name: "DATA_DIR", IsRequired: true}}, "/tmp/work")
	if env["API_KEY"] != Placeholder || env["MODE"] != "fast" || env["DATA_DIR"] != "/tmp/work" {
		t.Errorf("env = %v", env)
	}
	if _, ok := env["OPT"]; ok {
		t.Error("optional vars without defaults must be unset")
	}
}

func TestPickBin(t *testing.T) {
	cases := []struct {
		pkg   string
		bins  []string
		pref  string
		hints []string
		want  string
	}{
		{"@scope/server-x", []string{"cli", "server-x"}, "", nil, "server-x"},
		{"mcp_server_time", []string{"mcp-server-time"}, "", nil, "mcp-server-time"},
		{"tool", []string{"b", "a-mcp", "c"}, "", nil, "a-mcp"},
		{"tool", []string{"x", "y"}, "y", nil, "y"},
		{"tool", []string{"x", "y"}, "", []string{"x"}, "x"},
		{"tool", nil, "", nil, ""},
	}
	for _, c := range cases {
		if got := PickBin(c.pkg, c.bins, c.pref, c.hints); got != c.want {
			t.Errorf("PickBin(%s,%v)=%q want %q", c.pkg, c.bins, got, c.want)
		}
	}
}
