package sources

import (
	"encoding/json"
	"testing"
)

func TestParseRegistryItemShapes(t *testing.T) {
	wrapped := `{"server":{"name":"io.github.o/x","description":"d","version":"1.0.0","repository":{"url":"https://github.com/o/x","source":"github","subfolder":"src/x"},
		"packages":[{"registryType":"npm","identifier":"@o/x","version":"1.0.0","transport":{"type":"stdio"},"environmentVariables":[{"name":"K","isRequired":true,"isSecret":true}]}]},
		"_meta":{"io.modelcontextprotocol.registry/official":{"status":"active","isLatest":true,"publishedAt":"2026-01-02T03:04:05Z"}}}`
	s, meta, err := ParseRegistryItem(json.RawMessage(wrapped))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "io.github.o/x" || s.Repository.Subfolder != "src/x" || meta.Status != "active" || meta.IsLatest == nil || !*meta.IsLatest {
		t.Errorf("wrapped parse: %+v %+v", s, meta)
	}
	if len(s.Packages) != 1 || !s.Packages[0].EnvironmentVariables[0].IsSecret {
		t.Errorf("packages: %+v", s.Packages)
	}
	flat := `{"name":"io.github.o/y","version":"2","_meta":{"io.modelcontextprotocol.registry/official":{"status":"deprecated"}}}`
	s, meta, err = ParseRegistryItem(json.RawMessage(flat))
	if err != nil || s.Name != "io.github.o/y" || meta.Status != "deprecated" {
		t.Errorf("flat parse: %+v %+v %v", s, meta, err)
	}
	if _, _, err := ParseRegistryItem(json.RawMessage(`{"foo":1}`)); err == nil {
		t.Error("nameless item must error")
	}
}

func TestNpmDocHelpers(t *testing.T) {
	d := NpmVersionDoc{Name: "@scope/srv", Bin: json.RawMessage(`"dist/index.js"`), Dependencies: map[string]string{"@modelcontextprotocol/sdk": "^1"},
		Repository: json.RawMessage(`{"type":"git","url":"git+https://github.com/o/mono.git","directory":"packages/srv"}`)}
	if b := d.Bins(); len(b) != 1 || b[0] != "srv" {
		t.Errorf("bins = %v", b)
	}
	if d.MCPDep() != "@modelcontextprotocol/sdk" {
		t.Error("mcp dep not detected")
	}
	if u, dir := d.RepoInfo(); u != "git+https://github.com/o/mono.git" || dir != "packages/srv" {
		t.Errorf("repo info = %s %s", u, dir)
	}
	d.Bin = json.RawMessage(`{"b-cli":"x.js","a-cli":"y.js"}`)
	if b := d.Bins(); len(b) != 2 || b[0] != "a-cli" {
		t.Errorf("object bins = %v", b)
	}
}

func TestPyPIHelpers(t *testing.T) {
	var p PyPIProject
	p.Info.RequiresDist = []string{"httpx>=0.27", "pytest ; extra == 'dev'", "mcp[cli]>=1.2.0"}
	p.Info.ProjectURLs = map[string]string{"Homepage": "https://example.com", "Source": "https://github.com/o/r"}
	if p.MCPDep() != "mcp" {
		t.Error("mcp requirement not detected")
	}
	if p.RepoURL() != "https://github.com/o/r" {
		t.Errorf("repo url = %s", p.RepoURL())
	}
	p.Info.RequiresDist = []string{"mcp ; extra == 'server'"}
	if p.MCPDep() != "" {
		t.Error("optional extras must not count")
	}
	for name, want := range map[string]bool{"mcp-server-time": true, "fastmcp-x": false, "my_mcp": true, "mcpi": false, "weather-mcp-server": true} {
		if LooksLikeMCPName(name) != want {
			t.Errorf("LooksLikeMCPName(%s) != %v", name, want)
		}
	}
}
