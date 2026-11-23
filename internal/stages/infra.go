package stages

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/mcpkg/mcpkg/internal/queue"
	"github.com/mcpkg/mcpkg/internal/sandbox"
)

// infraRetry is how long a job waits when the worker itself is unhealthy.
const infraRetry = 10 * time.Minute

// deferIfInfra turns a worker-side failure (a sandbox infrastructure error or
// a full disk) into a deferral: the job is queued again after infraRetry
// without using an attempt. Any other error is returned unchanged and counts
// as a failed attempt. Without this, a worker whose disk filled up failed each
// job instantly and marked about 10,800 jobs dead in four minutes (issue N28).
func deferIfInfra(err error, now time.Time) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sandbox.ErrInfra) || errors.Is(err, syscall.ENOSPC) {
		return fmt.Errorf("worker problem, retrying without using an attempt: %w: %w", err, &queue.ErrRateLimited{Until: now.Add(infraRetry)})
	}
	return err
}
