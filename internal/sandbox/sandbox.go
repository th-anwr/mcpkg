// Package sandbox installs and starts untrusted MCP servers for smoke tests.
//
// Docker mode (production) uses two containers per job:
//  1. install: network limited to an allowlist proxy (npm/PyPI/GitHub only)
//  2. run:     --network none, read-only rootfs, no capabilities, memory/CPU/pid limits,
//     optional gVisor (--runtime runsc). The server only talks to us over stdio.
//
// Process mode runs on the host with a scrubbed environment. It is for local
// development only: it does NOT isolate untrusted code.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Job is one smoke-test run.
type Job struct {
	ID          string
	Kind        string // npm | pypi | oci
	Package     string
	Version     string
	PreferBin   string
	EntryPoints []string
	Args        []string
	Env         map[string]string
}

// Spec returns the npm/pip requirement string.
func (j Job) Spec() string {
	switch j.Kind {
	case "npm":
		if j.Version != "" {
			return j.Package + "@" + j.Version
		}
		return j.Package
	case "pypi":
		if j.Version != "" {
			return j.Package + "==" + j.Version
		}
		return j.Package
	case "oci":
		return j.Package
	}
	return j.Package
}

// Process is a started server.
type Process struct {
	Stdin   io.WriteCloser
	Stdout  io.ReadCloser
	Display string
	stderr  *LimitedBuffer
	stop    func()
	wait    func() error
	once    sync.Once
}

// Stderr returns captured stderr (bounded).
func (p *Process) Stderr() string { return p.stderr.String() }

// Close stops the server and waits for it.
func (p *Process) Close() {
	p.once.Do(func() {
		_ = p.Stdin.Close()
		done := make(chan struct{})
		go func() { _ = p.wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			p.stop()
			<-done
		}
	})
}

// Runner is implemented by Docker and Process sandboxes.
type Runner interface {
	Name() string
	// PathPlaceholder is a writable directory inside the sandbox for path arguments.
	PathPlaceholder(job Job) string
	// Install fetches the package (network allowed, restricted) and returns the bin to run.
	Install(ctx context.Context, job Job) (bin string, log string, err error)
	// Start launches the server with no network.
	Start(ctx context.Context, job Job, bin string) (*Process, error)
	Cleanup(job Job)
}

// ErrInstall marks a package that failed to install (counted against the tool).
var ErrInstall = errors.New("install failed")

// ErrInfra marks sandbox infrastructure problems (retried, not counted against the tool).
var ErrInfra = errors.New("sandbox infrastructure error")

// LimitedBuffer keeps the last N bytes written.
type LimitedBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

// NewLimitedBuffer returns a buffer that keeps at most max bytes (the tail).
func NewLimitedBuffer(max int) *LimitedBuffer { return &LimitedBuffer{max: max} }

func (b *LimitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *LimitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// parseMarker extracts `MCPKG_BINS=["a","b"]` style lines emitted by install scripts.
func parseMarker(out, marker string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, marker+"=") {
			return strings.TrimPrefix(line, marker+"=")
		}
	}
	return ""
}

// run executes a command, capturing combined output (bounded).
func run(ctx context.Context, name string, args []string, env []string, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = env
	}
	cmd.Dir = dir
	buf := NewLimitedBuffer(64 << 10)
	cmd.Stdout = buf
	cmd.Stderr = buf
	err := cmd.Run()
	if ctx.Err() != nil {
		return buf.String(), fmt.Errorf("%w: timed out", ErrInstall)
	}
	return buf.String(), err
}

// passthroughEnv are non-secret variables needed to reach package registries
// (proxy + CA bundle). Secrets from the worker's environment are never passed.
func passthroughEnv() []string {
	var out []string
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR",
		"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "PIP_CERT", "npm_config_cafile"} {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// display renders a command for humans (secrets are placeholders anyway).
func display(cmd string, args []string) string {
	var b bytes.Buffer
	b.WriteString(cmd)
	for _, a := range args {
		b.WriteByte(' ')
		if strings.ContainsAny(a, " \t\"'") {
			fmt.Fprintf(&b, "%q", a)
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}
