package identity

import "testing"

func TestParseRepo(t *testing.T) {
	cases := []struct {
		in, key string
		ok      bool
	}{
		{"https://github.com/Owner/Repo", "github.com/owner/repo", true},
		{"git+https://github.com/owner/repo.git", "github.com/owner/repo", true},
		{"git@github.com:owner/repo.git", "github.com/owner/repo", true},
		{"ssh://git@github.com/owner/repo", "github.com/owner/repo", true},
		{"github:owner/repo", "github.com/owner/repo", true},
		{"owner/repo", "github.com/owner/repo", true},
		{"https://www.github.com/owner/repo/", "github.com/owner/repo", true},
		{"https://github.com/modelcontextprotocol/servers/tree/main/src/time", "github.com/modelcontextprotocol/servers#src/time", true},
		{"https://github.com/o/r/blob/main/packages/x/README.md", "github.com/o/r#packages/x", true},
		{"https://gitlab.com/group/proj", "gitlab.com/group/proj", true},
		{"https://example.com/owner/repo", "", false},
		{"https://github.com/owner", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		r, ok := ParseRepo(c.in)
		if ok != c.ok {
			t.Errorf("ParseRepo(%q) ok=%v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && r.Key() != c.key {
			t.Errorf("ParseRepo(%q) key=%q, want %q", c.in, r.Key(), c.key)
		}
	}
}

func TestNames(t *testing.T) {
	if got := PyPIName("MCP_Server.Time"); got != "mcp-server-time" {
		t.Errorf("PyPIName = %q", got)
	}
	if got := OCIName("docker.io/mcp/github:1.2.3"); got != "mcp/github" {
		t.Errorf("OCIName = %q", got)
	}
	if got := OCIName("ghcr.io:443/org/img@sha256:abc"); got != "ghcr.io:443/org/img" {
		t.Errorf("OCIName with port/digest = %q", got)
	}
	if got := Slugify("io.github.foo/YouTube MCP"); got != "io.github.foo.youtube-mcp" {
		t.Errorf("Slugify = %q", got)
	}
	if got := Slugify("@scope/pkg"); got != "scope.pkg" {
		t.Errorf("Slugify scoped = %q", got)
	}
	if got := ServerKey("@modelcontextprotocol/server-everything"); got != "server-everything" {
		t.Errorf("ServerKey = %q", got)
	}
	if got := CleanSubfolder("./src/time/"); got != "src/time" {
		t.Errorf("CleanSubfolder = %q", got)
	}
	if got := CleanSubfolder("../etc"); got != "" {
		t.Errorf("CleanSubfolder traversal = %q", got)
	}
	if got := RemoteURL("HTTPS://Api.Example.com/mcp/?x=1#f"); got != "https://api.example.com/mcp" {
		t.Errorf("RemoteURL = %q", got)
	}
}
