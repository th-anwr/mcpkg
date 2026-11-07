package finder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mcpkg/mcpkg/internal/httpx"
)

func TestFinderServer(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/search":
			if r.URL.Query().Get("status") != "works" {
				t.Errorf("only_working should map to status=works: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"total":1,"results":[{"slug":"time","name":"io.github.x/time","summary":"Time tools","smokeStatus":"works","stars":5,"tools":["get_current_time"],"score":{"total":91,"grade":"A"}}]}`))
		case r.URL.Path == "/tool/time/install.md":
			_, _ = w.Write([]byte("# Install time\n\n## Method 1: PyPI\n\n### Claude Code\n\n```bash\nclaude mcp add time -- uvx mcp-server-time\n```\n\n### Cursor (~/.cursor/mcp.json)\n\n```json\n{}\n```\n\n## Verify\n- `get_current_time`\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	s := &Server{API: api.URL, Name: "mcpkg", Version: "test", HTTP: httpx.New("test")}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_tools","arguments":{"query":"time","only_working":true}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_install_instructions","arguments":{"slug":"time","client":"claude-code"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"nope"}`,
	}, "\n") + "\n"
	var out strings.Builder
	if err := s.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("want 5 responses (notification gets none), got %d:\n%s", len(lines), out.String())
	}
	var init struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	_ = json.Unmarshal([]byte(lines[0]), &init)
	if init.Result.ProtocolVersion != "2025-06-18" {
		t.Errorf("initialize: %s", lines[0])
	}
	if !strings.Contains(lines[1], "search_tools") || !strings.Contains(lines[1], "get_install_instructions") {
		t.Errorf("tools/list: %s", lines[1])
	}
	if !strings.Contains(lines[2], "slug: time") || !strings.Contains(lines[2], "A (91/100)") {
		t.Errorf("search: %s", lines[2])
	}
	if !strings.Contains(lines[3], "claude mcp add time") || strings.Contains(lines[3], "cursor/mcp.json") {
		t.Errorf("install should be filtered to Claude Code: %s", lines[3])
	}
	if !strings.Contains(lines[4], "-32601") {
		t.Errorf("unknown method: %s", lines[4])
	}
}
