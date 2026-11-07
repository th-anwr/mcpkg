// Package finder is a tiny stdio MCP server that lets any agent search the
// catalog and fetch install instructions: `mcpkg finder --api https://your-site`.
package finder

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/mcpkg/mcpkg/internal/httpx"
)

// Server answers MCP requests on stdin/stdout.
type Server struct {
	API     string // https://mcpkg.dev
	Name    string
	Version string
	HTTP    *httpx.Client
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

var tools = []map[string]any{
	{
		"name":        "search_tools",
		"title":       "Search MCP servers",
		"description": "Search a catalog of open-source MCP servers that were checked for repo health, security and whether they start. Returns name, slug, summary, trust grade, smoke-test status and install links. Prefer results with smokeStatus 'works' and grade A or B.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":        map[string]any{"type": "string", "description": "What the agent needs to do, e.g. 'youtube transcripts' or 'query postgres'"},
				"min_grade":    map[string]any{"type": "string", "enum": []string{"A", "B", "C", "D"}, "description": "Minimum trust grade"},
				"only_working": map[string]any{"type": "boolean", "description": "Only servers whose smoke test passed"},
				"limit":        map[string]any{"type": "integer", "minimum": 1, "maximum": 20},
			},
			"required": []string{"query"},
		},
		"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": true},
	},
	{
		"name":        "get_install_instructions",
		"title":       "Get install instructions",
		"description": "Get verified install instructions for one MCP server: which values to ask the user for (never invent API keys), ready-to-use config for a given client, and which tools should appear after installing.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"slug":   map[string]any{"type": "string", "description": "The slug from search_tools"},
				"client": map[string]any{"type": "string", "enum": []string{"claude-code", "claude-desktop", "cursor", "vscode", "codex"}, "description": "Target MCP client (default: all)"},
			},
			"required": []string{"slug"},
		},
		"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": true},
	},
}

// Serve runs until stdin closes.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}})
			continue
		}
		if len(req.ID) == 0 {
			continue // notification
		}
		result, rpcErr := s.handle(ctx, req)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) handle(ctx context.Context, req request) (any, map[string]any) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := p.ProtocolVersion
		if version == "" {
			version = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.Name + "-finder", "version": s.Version},
			"instructions":    "Use search_tools to find an MCP server for a task, then get_install_instructions. Ask the user for any required API keys; never invent them.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, map[string]any{"code": -32602, "message": "invalid params"}
		}
		text, isErr := s.call(ctx, p.Name, p.Arguments)
		return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}, nil
	}
	return nil, map[string]any{"code": -32601, "message": "method not found: " + req.Method}
}

func (s *Server) call(ctx context.Context, name string, args json.RawMessage) (string, bool) {
	switch name {
	case "search_tools":
		var a struct {
			Query       string `json:"query"`
			MinGrade    string `json:"min_grade"`
			OnlyWorking bool   `json:"only_working"`
			Limit       int    `json:"limit"`
		}
		_ = json.Unmarshal(args, &a)
		if strings.TrimSpace(a.Query) == "" {
			return "query is required", true
		}
		q := url.Values{"q": {a.Query}}
		if a.MinGrade != "" {
			q.Set("grade", a.MinGrade)
		}
		if a.OnlyWorking {
			q.Set("status", "works")
		}
		limit := a.Limit
		if limit <= 0 || limit > 20 {
			limit = 8
		}
		q.Set("limit", fmt.Sprint(limit))
		var res struct {
			Total   int `json:"total"`
			Results []struct {
				Slug        string   `json:"slug"`
				Name        string   `json:"name"`
				Summary     string   `json:"summary"`
				SmokeStatus string   `json:"smokeStatus"`
				Stars       int      `json:"stars"`
				Tools       []string `json:"tools"`
				Score       *struct {
					Total int    `json:"total"`
					Grade string `json:"grade"`
				} `json:"score"`
				InstallMD string `json:"installMdUrl"`
			} `json:"results"`
		}
		if err := s.HTTP.GetJSON(ctx, s.API+"/api/search?"+q.Encode(), nil, &res); err != nil {
			return "search failed: " + err.Error(), true
		}
		if len(res.Results) == 0 {
			return "No matching servers. Try different words, or drop the filters.", false
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d matching servers (showing %d):\n", res.Total, len(res.Results))
		for _, r := range res.Results {
			grade := "unscored"
			if r.Score != nil {
				grade = fmt.Sprintf("%s (%d/100)", r.Score.Grade, r.Score.Total)
			}
			fmt.Fprintf(&b, "\n- slug: %s\n  name: %s\n  summary: %s\n  trust: %s, smoke test: %s, stars: %d\n", r.Slug, r.Name, r.Summary, grade, r.SmokeStatus, r.Stars)
			if len(r.Tools) > 0 {
				shown := r.Tools
				if len(shown) > 10 {
					shown = shown[:10]
				}
				fmt.Fprintf(&b, "  tools: %s\n", strings.Join(shown, ", "))
			}
		}
		b.WriteString("\nNext: call get_install_instructions with a slug.")
		return b.String(), false
	case "get_install_instructions":
		var a struct {
			Slug   string `json:"slug"`
			Client string `json:"client"`
		}
		_ = json.Unmarshal(args, &a)
		if a.Slug == "" {
			return "slug is required", true
		}
		data, _, err := s.HTTP.Do(ctx, httpx.Request{URL: s.API + "/tool/" + url.PathEscape(a.Slug) + "/install.md"})
		if err != nil {
			return "could not load install instructions: " + err.Error(), true
		}
		md := string(data)
		if a.Client != "" {
			md = filterClient(md, a.Client)
		}
		return md, false
	}
	return "unknown tool " + name, true
}

// filterClient keeps the header, env table and only the requested client's blocks.
func filterClient(md, client string) string {
	label := map[string]string{"claude-code": "Claude Code", "claude-desktop": "Claude Desktop", "cursor": "Cursor", "vscode": "VS Code", "codex": "Codex CLI"}[client]
	if label == "" {
		return md
	}
	var out []string
	skipping := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "### ") {
			skipping = !strings.HasPrefix(strings.TrimPrefix(line, "### "), label)
		} else if strings.HasPrefix(line, "## ") {
			skipping = false
		}
		if !skipping {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
