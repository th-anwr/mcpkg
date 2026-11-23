package stages

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"testing"
	"time"

	"github.com/mcpkg/mcpkg/internal/queue"
	"github.com/mcpkg/mcpkg/internal/sandbox"
)

func TestDeferIfInfra(t *testing.T) {
	now := time.Date(2026, 11, 11, 16, 45, 0, 0, time.UTC)
	cases := []struct {
		name      string
		err       error
		wantDefer bool
	}{
		{"sandbox infrastructure", fmt.Errorf("%w: docker volume create: exit status 1", sandbox.ErrInfra), true},
		{"disk full", &fs.PathError{Op: "mkdir", Path: "/tmp/mcpkg-sec-1", Err: syscall.ENOSPC}, true},
		{"disk full, wrapped", fmt.Errorf("fetch: %w", &fs.PathError{Op: "write", Path: "/tmp/x", Err: syscall.ENOSPC}), true},
		{"package problem", errors.New("extract wheel: archive exceeds extraction limits"), false},
		{"install failure", fmt.Errorf("%w: exit status 1", sandbox.ErrInstall), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deferIfInfra(c.err, now)
			rl, deferred := queue.AsRateLimited(got)
			if deferred != c.wantDefer {
				t.Fatalf("deferred = %v, want %v (err: %v)", deferred, c.wantDefer, got)
			}
			if !c.wantDefer {
				if got != c.err {
					t.Errorf("a non-infrastructure error must be returned unchanged, got %v", got)
				}
				return
			}
			if !rl.Until.Equal(now.Add(infraRetry)) {
				t.Errorf("retry at %v, want %v", rl.Until, now.Add(infraRetry))
			}
			if !errors.Is(got, c.err) {
				t.Errorf("the original error must stay in the chain: %v", got)
			}
		})
	}
	if deferIfInfra(nil, now) != nil {
		t.Error("nil must stay nil")
	}
}
