package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SafeHTTPClient refuses to connect to loopback/private/link-local/metadata
// addresses, so smoke-testing a "remote" URL can't be used for SSRF into our VPC
// (e.g. 169.254.169.254). allowPrivate is for tests only.
func SafeHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			if allowPrivate {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || !IsPublicIP(ip) {
				return fmt.Errorf("refusing to connect to non-public address %s", host)
			}
			return nil
		},
	}
	tr := &http.Transport{
		Proxy:                 nil, // never route remote checks through internal proxies
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          10,
	}
	return &http.Client{Timeout: timeout, Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		return nil
	}}
}

var cgnat = mustCIDR("100.64.0.0/10")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// IsPublicIP reports whether ip is globally routable.
func IsPublicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() || cgnat.Contains(ip))
}

// HandshakeHTTP probes a remote server: Streamable HTTP first, legacy SSE as fallback
// (or directly when kind == "sse").
func HandshakeHTTP(ctx context.Context, client *http.Client, endpoint, kind string, headers map[string]string) (*Info, error) {
	return HandshakeHTTPLog(ctx, client, endpoint, kind, headers, nil)
}

// HandshakeHTTPLog is HandshakeHTTP with an optional logger for handshake progress.
func HandshakeHTTPLog(ctx context.Context, client *http.Client, endpoint, kind string, headers map[string]string, log *slog.Logger) (*Info, error) {
	if kind == "sse" {
		return handshakeSSE(ctx, client, endpoint, headers, log)
	}
	h := &streamable{client: client, url: endpoint, headers: headers, log: log}
	info, err := handshake(ctx, h, log)
	var se *statusError
	if err != nil && errors.As(err, &se) && (se.code == 404 || se.code == 405 || se.code == 400) && h.session == "" {
		if info2, err2 := handshakeSSE(ctx, client, endpoint, headers, log); err2 == nil {
			return info2, nil
		}
	}
	return info, err
}

type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.code, e.body) }

// ---- Streamable HTTP ----

type streamable struct {
	client   *http.Client
	url      string
	headers  map[string]string
	session  string
	protocol string
	nextID   int
	log      *slog.Logger
}

func (s *streamable) polluted() bool { return false }

func (s *streamable) post(ctx context.Context, body any) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", ClientName+"/0.1")
	if s.session != "" {
		req.Header.Set("Mcp-Session-Id", s.session)
	}
	if s.protocol != "" {
		req.Header.Set("MCP-Protocol-Version", s.protocol)
	}
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		resp.Body.Close()
		return nil, ErrAuthRequired
	}
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, &statusError{code: resp.StatusCode, body: strings.TrimSpace(string(data))}
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		s.session = sid
	}
	return resp, nil
}

func (s *streamable) notify(ctx context.Context, method string, params any) error {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		m["params"] = params
	}
	resp, err := s.post(ctx, m)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s *streamable) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.nextID++
	id := fmt.Sprint(s.nextID)
	if s.log != nil {
		s.log.Debug("mcp rpc", "transport", "streamable-http", "method", method)
	}
	m := map[string]any{"jsonrpc": "2.0", "id": s.nextID, "method": method}
	if params != nil {
		m["params"] = params
	}
	resp, err := s.post(ctx, m)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	var msgs []message
	if strings.HasPrefix(ct, "text/event-stream") {
		err = readSSE(ctx, resp.Body, func(event, data string) bool {
			var m message
			if json.Unmarshal([]byte(data), &m) == nil {
				msgs = append(msgs, m)
				return strings.Trim(string(m.ID), `"`) == id && m.Method == ""
			}
			return false
		})
		if err != nil && len(msgs) == 0 {
			return nil, err
		}
	} else {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return nil, err
		}
		data = bytes.TrimSpace(data)
		if len(data) > 0 && data[0] == '[' {
			_ = json.Unmarshal(data, &msgs)
		} else {
			var m message
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("invalid JSON-RPC response: %w", err)
			}
			msgs = append(msgs, m)
		}
	}
	for _, m := range msgs {
		if m.Method == "" && strings.Trim(string(m.ID), `"`) == id {
			if m.Error != nil {
				return nil, m.Error
			}
			if method == "initialize" {
				var r struct {
					ProtocolVersion string `json:"protocolVersion"`
				}
				_ = json.Unmarshal(m.Result, &r)
				s.protocol = r.ProtocolVersion
			}
			return m.Result, nil
		}
	}
	return nil, fmt.Errorf("no response for request %s", id)
}

// readSSE parses an event stream, calling fn per event until it returns true.
func readSSE(ctx context.Context, r io.Reader, fn func(event, data string) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var event string
	var data []string
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 || event != "" {
				if fn(event, strings.Join(data, "\n")) {
					return nil
				}
			}
			event, data = "", nil
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}

// ---- legacy HTTP+SSE (protocol 2024-11-05) ----

type sseTransport struct {
	client  *http.Client
	post    string
	headers map[string]string
	events  chan message
	mu      sync.Mutex
	nextID  int
	log     *slog.Logger
}

func handshakeSSE(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, log *slog.Logger) (*Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// The SSE stream stays open, so use a client without an overall timeout; ctx bounds it.
	streamClient := *client
	streamClient.Timeout = 0
	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, ErrAuthRequired
	}
	if resp.StatusCode >= 400 {
		return nil, &statusError{code: resp.StatusCode}
	}
	t := &sseTransport{client: client, headers: headers, events: make(chan message, 32), log: log}
	endpointCh := make(chan string, 1)
	go func() {
		defer close(t.events)
		_ = readSSE(ctx, resp.Body, func(event, data string) bool {
			if event == "endpoint" {
				select {
				case endpointCh <- data:
				default:
				}
				return false
			}
			var m message
			if json.Unmarshal([]byte(data), &m) == nil {
				select {
				case t.events <- m:
				case <-ctx.Done():
					return true
				}
			}
			return false
		})
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case ep := <-endpointCh:
		base, _ := url.Parse(endpoint)
		ref, err := url.Parse(strings.TrimSpace(ep))
		if err != nil {
			return nil, err
		}
		t.post = base.ResolveReference(ref).String()
	}
	return handshake(ctx, t, log)
}

func (t *sseTransport) polluted() bool { return false }

func (t *sseTransport) send(ctx context.Context, m any) error {
	b, _ := json.Marshal(m)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.post, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return ErrAuthRequired
	}
	if resp.StatusCode >= 400 {
		return &statusError{code: resp.StatusCode}
	}
	return nil
}

func (t *sseTransport) notify(ctx context.Context, method string, params any) error {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		m["params"] = params
	}
	return t.send(ctx, m)
}

func (t *sseTransport) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	t.mu.Lock()
	t.nextID++
	id := fmt.Sprint(t.nextID)
	t.mu.Unlock()
	m := map[string]any{"jsonrpc": "2.0", "id": t.nextID, "method": method}
	if params != nil {
		m["params"] = params
	}
	if err := t.send(ctx, m); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case msg, ok := <-t.events:
			if !ok {
				return nil, ErrClosed
			}
			if msg.Method != "" && len(msg.ID) > 0 {
				_ = t.send(ctx, answerServerRequest(msg))
				continue
			}
			if msg.Method == "" && strings.Trim(string(msg.ID), `"`) == id {
				if msg.Error != nil {
					return nil, msg.Error
				}
				return msg.Result, nil
			}
		}
	}
}
