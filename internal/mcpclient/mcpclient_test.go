package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeStdioServer speaks MCP over pipes, printing a log line to stdout first (pollution)
// and issuing a roots/list request to the client before answering tools/list.
func fakeStdioServer(t *testing.T, in io.Reader, out io.Writer) {
	sc := bufio.NewScanner(in)
	fmt.Fprintln(out, "server starting on stdio...") // bad practice, should be flagged
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		id, hasID := m["id"]
		switch m["method"] {
		case "initialize":
			fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%v,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{},"prompts":{}},"serverInfo":{"name":"fake","version":"9.9"}}}`+"\n", id)
		case "tools/list":
			fmt.Fprintln(out, `{"jsonrpc":"2.0","id":"srv-1","method":"roots/list"}`)
			fmt.Fprintf(out, `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"hi"}}`+"\n")
			fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%v,"result":{"tools":[{"name":"add","description":"Add numbers","inputSchema":{"type":"object","properties":{"a":{"type":"number"}},"required":["a"]}}]}}`+"\n", id)
		case "prompts/list":
			fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%v,"result":{"prompts":[{"name":"p1"},{"name":"p2"}]}}`+"\n", id)
		default:
			if hasID && m["method"] != nil {
				fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%v,"error":{"code":-32601,"message":"nope"}}`+"\n", id)
			}
		}
	}
}

func TestHandshakeStdio(t *testing.T) {
	cr, cw := io.Pipe() // client -> server
	sr, sw := io.Pipe() // server -> client
	go fakeStdioServer(t, cr, sw)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := HandshakeStdio(ctx, cw, sr)
	if err != nil {
		t.Fatal(err)
	}
	if info.ServerName != "fake" || info.ProtocolVersion != "2025-06-18" {
		t.Errorf("info = %+v", info)
	}
	if len(info.Tools) != 1 || info.Tools[0].Name != "add" || info.PromptsCount != 2 {
		t.Errorf("tools/prompts = %+v / %d", info.Tools, info.PromptsCount)
	}
	if !info.StdoutPollution {
		t.Error("stdout pollution should be detected")
	}
}

func TestHandshakeStdioServerExits(t *testing.T) {
	cr, cw := io.Pipe()
	sr, sw := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, cr) }()
	_ = sw.Close() // server died immediately
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := HandshakeStdio(ctx, cw, sr)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

func streamableHandler(useSSE bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		id, hasID := m["id"]
		if !hasID {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result string
		switch m["method"] {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			result = `{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"remote","version":"1"}}`
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "sess-1" {
				http.Error(w, "missing session", 400)
				return
			}
			result = `{"tools":[{"name":"search"}]}`
		default:
			result = `{}`
		}
		msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%v,"result":%s}`, id, result)
		if useSSE {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\nevent: message\ndata: %s\n\n", msg)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(msg))
	}
}

func TestHandshakeHTTP(t *testing.T) {
	for _, sse := range []bool{false, true} {
		srv := httptest.NewServer(streamableHandler(sse))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		info, err := HandshakeHTTP(ctx, SafeHTTPClient(5*time.Second, true), srv.URL, "streamable-http", nil)
		cancel()
		srv.Close()
		if err != nil {
			t.Fatalf("sse=%v: %v", sse, err)
		}
		if info.ServerName != "remote" || len(info.Tools) != 1 {
			t.Errorf("sse=%v: info = %+v", sse, info)
		}
	}
}

func TestHandshakeHTTPAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(401)
	}))
	defer srv.Close()
	_, err := HandshakeHTTP(context.Background(), SafeHTTPClient(5*time.Second, true), srv.URL, "streamable-http", nil)
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("want ErrAuthRequired, got %v", err)
	}
}

func TestLegacySSE(t *testing.T) {
	msgs := make(chan string, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "event: endpoint\ndata: /messages?session=abc\n\n")
		fl.Flush()
		for {
			select {
			case m := <-msgs:
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", m)
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		w.WriteHeader(202)
		id, ok := m["id"]
		if !ok {
			return
		}
		switch m["method"] {
		case "initialize":
			msgs <- fmt.Sprintf(`{"jsonrpc":"2.0","id":%v,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"legacy","version":"0"}}}`, id)
		case "tools/list":
			msgs <- fmt.Sprintf(`{"jsonrpc":"2.0","id":%v,"result":{"tools":[{"name":"t1"},{"name":"t2"}]}}`, id)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := HandshakeHTTP(ctx, SafeHTTPClient(5*time.Second, true), srv.URL+"/sse", "sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.ServerName != "legacy" || len(info.Tools) != 2 {
		t.Errorf("info = %+v", info)
	}
}

func TestSafeClientBlocksPrivate(t *testing.T) {
	srv := httptest.NewServer(streamableHandler(false))
	defer srv.Close()
	_, err := HandshakeHTTP(context.Background(), SafeHTTPClient(5*time.Second, false), srv.URL, "streamable-http", nil)
	if err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("expected SSRF guard to block loopback, got %v", err)
	}
	for ip, public := range map[string]bool{"8.8.8.8": true, "10.1.2.3": false, "169.254.169.254": false, "100.64.0.1": false, "::1": false, "2606:4700::1111": true} {
		if IsPublicIP(net.ParseIP(ip)) != public {
			t.Errorf("IsPublicIP(%s) != %v", ip, public)
		}
	}
}
