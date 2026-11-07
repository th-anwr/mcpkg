// Package mcpclient is a minimal MCP client used by the smoke test: it performs
// initialize -> notifications/initialized -> tools/list (+ counts prompts/resources)
// over stdio, Streamable HTTP, or legacy HTTP+SSE. It never calls tools.
package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mcpkg/mcpkg/internal/model"
)

// LatestProtocol is the protocol version we request; servers may answer with an older one.
const LatestProtocol = "2025-11-25"

// ClientName is sent as clientInfo.name.
const ClientName = "mcpkg-smoke"

// Info is what we learn from a handshake.
type Info struct {
	ProtocolVersion string
	ServerName      string
	ServerVersion   string
	Capabilities    []string
	Tools           []model.ToolInfo
	PromptsCount    int
	ResourcesCount  int
	StdoutPollution bool
	InitMs          int64
	ToolsError      string
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// ErrAuthRequired means the endpoint wants credentials (HTTP 401/403).
var ErrAuthRequired = errors.New("authentication required")

// ErrClosed means the server exited / closed the stream before answering.
var ErrClosed = errors.New("server closed the connection")

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// transport abstracts stdio/http.
type transport interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
	polluted() bool
}

// Handshake runs the standard discovery sequence on a transport.
func handshake(ctx context.Context, tr transport) (*Info, error) {
	start := time.Now()
	raw, err := tr.call(ctx, "initialize", map[string]any{
		"protocolVersion": LatestProtocol,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": ClientName, "version": "0.1.0"},
	})
	if err != nil {
		return nil, err
	}
	var init struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(raw, &init); err != nil {
		return nil, fmt.Errorf("invalid initialize result: %w", err)
	}
	info := &Info{ProtocolVersion: init.ProtocolVersion, ServerName: init.ServerInfo.Name, ServerVersion: init.ServerInfo.Version,
		InitMs: time.Since(start).Milliseconds()}
	for k := range init.Capabilities {
		info.Capabilities = append(info.Capabilities, k)
	}
	sort.Strings(info.Capabilities)
	if err := tr.notify(ctx, "notifications/initialized", nil); err != nil {
		return info, err
	}
	_, hasTools := init.Capabilities["tools"]
	if hasTools || len(init.Capabilities) == 0 {
		tools, err := listAll(ctx, tr, "tools/list", "tools")
		if err != nil {
			info.ToolsError = err.Error()
			if errors.Is(err, ErrAuthRequired) || errors.Is(err, ErrClosed) || ctx.Err() != nil {
				return info, err
			}
		}
		for _, t := range tools {
			var ti model.ToolInfo
			if json.Unmarshal(t, &ti) == nil && ti.Name != "" {
				ti.Description = truncate(ti.Description, 4000)
				if len(ti.InputSchema) > 8000 {
					ti.InputSchema = nil
				}
				info.Tools = append(info.Tools, ti)
			}
		}
	}
	if _, ok := init.Capabilities["prompts"]; ok {
		if items, err := listAll(ctx, tr, "prompts/list", "prompts"); err == nil {
			info.PromptsCount = len(items)
		}
	}
	if _, ok := init.Capabilities["resources"]; ok {
		if items, err := listAll(ctx, tr, "resources/list", "resources"); err == nil {
			info.ResourcesCount = len(items)
		}
	}
	info.StdoutPollution = tr.polluted()
	return info, nil
}

func listAll(ctx context.Context, tr transport, method, key string) ([]json.RawMessage, error) {
	var all []json.RawMessage
	cursor := ""
	for page := 0; page < 20; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := tr.call(ctx, method, params)
		if err != nil {
			var re *RPCError
			if errors.As(err, &re) && re.Code == -32601 {
				return all, nil // method not found: capability absent
			}
			return all, err
		}
		var res map[string]json.RawMessage
		if err := json.Unmarshal(raw, &res); err != nil {
			return all, err
		}
		var items []json.RawMessage
		_ = json.Unmarshal(res[key], &items)
		all = append(all, items...)
		var next string
		_ = json.Unmarshal(res["nextCursor"], &next)
		if next == "" || len(all) > 2000 {
			break
		}
		cursor = next
	}
	return all, nil
}

// ---- stdio ----

type stdio struct {
	w      io.Writer
	lines  chan []byte
	done   chan struct{}
	mu     sync.Mutex
	nextID int
	dirty  bool
}

// HandshakeStdio talks newline-delimited JSON-RPC over a child's stdin/stdout.
func HandshakeStdio(ctx context.Context, stdin io.Writer, stdout io.Reader) (*Info, error) {
	s := &stdio{w: stdin, lines: make(chan []byte, 64), done: make(chan struct{})}
	go func() {
		defer close(s.lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			select {
			case s.lines <- line:
			case <-s.done:
				return
			}
		}
	}()
	defer close(s.done)
	return handshake(ctx, s)
}

func (s *stdio) polluted() bool { return s.dirty }

func (s *stdio) write(m any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.w.Write(append(b, '\n'))
	return err
}

func (s *stdio) notify(ctx context.Context, method string, params any) error {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		m["params"] = params
	}
	return s.write(m)
}

func (s *stdio) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.nextID++
	id := s.nextID
	m := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	if err := s.write(m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrClosed, err)
	}
	want := fmt.Sprint(id)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case line, ok := <-s.lines:
			if !ok {
				return nil, ErrClosed
			}
			reply, handled := s.handle(line, want)
			if handled {
				if reply.err != nil {
					return nil, reply.err
				}
				return reply.result, nil
			}
		}
	}
}

type reply struct {
	result json.RawMessage
	err    error
}

// handle processes one inbound line; returns (reply, true) when it answers our request.
func (s *stdio) handle(line []byte, want string) (reply, bool) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return reply{}, false
	}
	var m message
	if !strings.HasPrefix(trimmed, "{") || json.Unmarshal([]byte(trimmed), &m) != nil || m.JSONRPC == "" {
		s.dirty = true // logs printed to stdout break real clients
		return reply{}, false
	}
	if m.Method != "" {
		if len(m.ID) > 0 { // server -> client request
			_ = s.write(answerServerRequest(m))
		}
		return reply{}, false
	}
	if strings.Trim(string(m.ID), `"`) != want {
		return reply{}, false
	}
	if m.Error != nil {
		return reply{err: m.Error}, true
	}
	return reply{result: m.Result}, true
}

func answerServerRequest(m message) map[string]any {
	out := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	switch m.Method {
	case "ping":
		out["result"] = map[string]any{}
	case "roots/list":
		out["result"] = map[string]any{"roots": []any{}}
	default:
		out["error"] = map[string]any{"code": -32601, "message": "not supported by " + ClientName}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
