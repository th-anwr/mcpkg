// Package logx builds the process logger: human-readable text on stderr plus
// JSON lines in a size-rotated file, so logs survive restarts and can be
// grepped or shipped later. No dependencies beyond the standard library.
//
// Env:
//
//	LOG_DIR       directory for log files (default "logs"; "-" disables the file)
//	LOG_MAX_MB    rotate when the file exceeds this many MiB (default 50)
//	LOG_KEEP      rotated files to keep (default 5)
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// New returns a logger at the given level. name is the file stem, usually
// "mcpkg" or the worker id. Failure to open the file is reported on stderr
// and logging continues to stderr only.
func New(level, name string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	opts := &slog.HandlerOptions{Level: l}

	stderr := slog.NewTextHandler(os.Stderr, opts)
	dir := envOr("LOG_DIR", "logs")
	if dir == "-" {
		return slog.New(stderr)
	}
	f, err := Open(filepath.Join(dir, name+".log"), envInt("LOG_MAX_MB", 50)<<20, envInt("LOG_KEEP", 5))
	if err != nil {
		fmt.Fprintln(os.Stderr, "logx: file logging disabled:", err)
		return slog.New(stderr)
	}
	return slog.New(tee{stderr, slog.NewJSONHandler(f, opts)})
}

// File is an io.Writer that appends to path and rotates it to path.1, path.2,
// … once it exceeds maxBytes. Safe for concurrent use.
type File struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

// Open creates the parent directory if needed and opens path for append.
func Open(path string, maxBytes int64, keep int) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	w := &File{path: path, maxBytes: maxBytes, keep: keep}
	return w, w.open()
}

func (w *File) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, st.Size()
	return nil
}

func (w *File) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.maxBytes > 0 && w.size+int64(len(p)) > w.maxBytes && w.size > 0 {
		w.rotate()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *File) rotate() {
	w.f.Close()
	for i := w.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
	}
	if w.keep > 0 {
		os.Rename(w.path, w.path+".1")
	} else {
		os.Remove(w.path)
	}
	if err := w.open(); err != nil {
		w.f, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		w.size = 0
	}
}

// Close closes the underlying file.
func (w *File) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

var _ io.Writer = (*File)(nil)

// tee fans a record out to several handlers.
type tee []slog.Handler

func (t tee) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range t {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (t tee) Handle(ctx context.Context, r slog.Record) error {
	var first error
	for _, h := range t {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func (t tee) WithAttrs(a []slog.Attr) slog.Handler {
	out := make(tee, len(t))
	for i, h := range t {
		out[i] = h.WithAttrs(a)
	}
	return out
}

func (t tee) WithGroup(g string) slog.Handler {
	out := make(tee, len(t))
	for i, h := range t {
		out[i] = h.WithGroup(g)
	}
	return out
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int64) int64 {
	if v, err := strconv.ParseInt(os.Getenv(k), 10, 64); err == nil && v > 0 {
		return v
	}
	return d
}
