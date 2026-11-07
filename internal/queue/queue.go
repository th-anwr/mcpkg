// Package queue is a small Postgres job queue built on FOR UPDATE SKIP LOCKED.
// Many workers on many machines can claim jobs safely; a crashed or
// spot-interrupted worker's jobs come back automatically when its lease expires.
package queue

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"time"
)

// Job is a claimed unit of work: run one pipeline stage for one tool.
type Job struct {
	ID          int64
	Stage       string
	ToolID      int64
	Payload     json.RawMessage
	Attempts    int
	MaxAttempts int
}

// Queue wraps the jobs table.
type Queue struct {
	DB  *sql.DB
	Log *slog.Logger // optional: retries and dead jobs are logged here
}

func (q *Queue) log() *slog.Logger {
	if q.Log != nil {
		return q.Log
	}
	return discard
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// New returns a queue on the given connection.
func New(db *sql.DB) *Queue { return &Queue{DB: db} }

// Enqueue adds a job unless an active (queued/running) job for the same
// stage+tool already exists. Returns true if a new job was created.
func (q *Queue) Enqueue(ctx context.Context, stage string, toolID int64, runAfter time.Time) (bool, error) {
	return q.EnqueuePriority(ctx, stage, toolID, runAfter, 0)
}

// EnqueuePriority is Enqueue with a priority: higher runs first among due jobs.
func (q *Queue) EnqueuePriority(ctx context.Context, stage string, toolID int64, runAfter time.Time, priority int) (bool, error) {
	if runAfter.IsZero() {
		runAfter = time.Now()
	}
	res, err := q.DB.ExecContext(ctx, `
		INSERT INTO jobs (stage, tool_id, run_after, priority) VALUES ($1, $2, $3, $4)
		ON CONFLICT (stage, tool_id) WHERE state IN ('queued','running') DO NOTHING`,
		stage, toolID, runAfter, priority)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// EnqueueMany enqueues a stage for many tools in one statement (batch adds).
func (q *Queue) EnqueueMany(ctx context.Context, stage string, toolIDs []int64, runAfter time.Time) (int64, error) {
	if len(toolIDs) == 0 {
		return 0, nil
	}
	if runAfter.IsZero() {
		runAfter = time.Now()
	}
	res, err := q.DB.ExecContext(ctx, `
		INSERT INTO jobs (stage, tool_id, run_after)
		SELECT $1, unnest($2::bigint[]), $3
		ON CONFLICT (stage, tool_id) WHERE state IN ('queued','running') DO NOTHING`,
		stage, int64Array(toolIDs), runAfter)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Claim atomically takes up to n runnable jobs for a stage and leases them.
func (q *Queue) Claim(ctx context.Context, stage string, n int, worker string, lease time.Duration) ([]Job, error) {
	rows, err := q.DB.QueryContext(ctx, `
		UPDATE jobs SET state='running', locked_by=$3, locked_until=now() + make_interval(secs => $4),
		       attempts=attempts+1, updated_at=now()
		WHERE id IN (
			SELECT id FROM jobs
			WHERE stage=$1 AND state='queued' AND run_after <= now()
			ORDER BY priority DESC, run_after, id
			LIMIT $2
			FOR UPDATE SKIP LOCKED)
		RETURNING id, stage, tool_id, payload, attempts, max_attempts`,
		stage, n, worker, lease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Stage, &j.ToolID, &j.Payload, &j.Attempts, &j.MaxAttempts); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// Complete marks a job done.
func (q *Queue) Complete(ctx context.Context, id int64) error {
	_, err := q.DB.ExecContext(ctx, `UPDATE jobs SET state='done', locked_by=NULL, locked_until=NULL, updated_at=now() WHERE id=$1`, id)
	return err
}

// Fail records an error and either re-queues with exponential backoff or,
// after max attempts, parks the job as 'dead' for inspection.
func (q *Queue) Fail(ctx context.Context, j Job, cause error) error {
	msg := "unknown error"
	if cause != nil {
		msg = cause.Error()
		if len(msg) > 2000 {
			msg = msg[:2000]
		}
	}
	if j.Attempts >= j.MaxAttempts {
		q.log().Warn("job dead", "stage", j.Stage, "tool_id", j.ToolID, "job", j.ID, "attempts", j.Attempts, "err", msg)
		_, err := q.DB.ExecContext(ctx, `UPDATE jobs SET state='dead', last_error=$2, locked_by=NULL, locked_until=NULL, updated_at=now() WHERE id=$1`, j.ID, msg)
		return err
	}
	delay := Backoff(j.Attempts)
	q.log().Info("job retry", "stage", j.Stage, "tool_id", j.ToolID, "job", j.ID, "attempt", j.Attempts, "in", delay.Round(time.Second).String())
	_, err := q.DB.ExecContext(ctx, `
		UPDATE jobs SET state='queued', last_error=$2, locked_by=NULL, locked_until=NULL,
		       run_after=now() + make_interval(secs => $3), updated_at=now()
		WHERE id=$1`, j.ID, msg, delay.Seconds())
	return err
}

// RetryAfter re-queues a job without counting the attempt (e.g. rate limited).
func (q *Queue) RetryAfter(ctx context.Context, j Job, d time.Duration, reason string) error {
	q.log().Info("job deferred", "stage", j.Stage, "tool_id", j.ToolID, "job", j.ID, "in", d.Round(time.Second).String(), "reason", reason)
	_, err := q.DB.ExecContext(ctx, `
		UPDATE jobs SET state='queued', attempts=GREATEST(attempts-1,0), last_error=$2, locked_by=NULL, locked_until=NULL,
		       run_after=now() + make_interval(secs => $3), updated_at=now()
		WHERE id=$1`, j.ID, reason, d.Seconds())
	return err
}

// ReapExpired returns jobs whose lease expired (worker died / spot interruption) to the queue.
func (q *Queue) ReapExpired(ctx context.Context) (int64, error) {
	res, err := q.DB.ExecContext(ctx, `
		UPDATE jobs SET state='queued', locked_by=NULL, locked_until=NULL, updated_at=now(),
		       last_error=coalesce(last_error,'') || ' [lease expired]'
		WHERE state='running' AND locked_until < now()`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeadTTL is how long dead jobs stay for inspection (status.sh, last_error).
// The scheduler's stale re-check enqueues those tools again anyway.
const DeadTTL = 30 * 24 * time.Hour

// Cleanup deletes finished jobs older than the given age and dead jobs older
// than DeadTTL, so the jobs table does not grow without bound.
func (q *Queue) Cleanup(ctx context.Context, olderThan time.Duration) (int64, error) {
	res, err := q.DB.ExecContext(ctx, `DELETE FROM jobs
		WHERE (state='done' AND updated_at < now() - make_interval(secs => $1))
		   OR (state='dead' AND updated_at < now() - make_interval(secs => $2))`, olderThan.Seconds(), DeadTTL.Seconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// StageStats is a per-stage count of jobs by state.
type StageStats struct {
	Stage   string
	Queued  int64
	Due     int64
	Running int64
	Dead    int64
	Done    int64
}

// Stats reports queue depth per stage.
func (q *Queue) Stats(ctx context.Context) ([]StageStats, error) {
	rows, err := q.DB.QueryContext(ctx, `
		SELECT stage,
		  count(*) FILTER (WHERE state='queued'),
		  count(*) FILTER (WHERE state='queued' AND run_after <= now()),
		  count(*) FILTER (WHERE state='running'),
		  count(*) FILTER (WHERE state='dead'),
		  count(*) FILTER (WHERE state='done')
		FROM jobs GROUP BY stage ORDER BY stage`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StageStats
	for rows.Next() {
		var s StageStats
		if err := rows.Scan(&s.Stage, &s.Queued, &s.Due, &s.Running, &s.Dead, &s.Done); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Backoff is 1m, 4m, 16m, ... capped at 6h.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Duration(math.Pow(4, float64(attempt-1))) * time.Minute
	if d > 6*time.Hour {
		d = 6 * time.Hour
	}
	return d
}

// ErrRateLimited signals the caller should retry later without burning an attempt.
type ErrRateLimited struct{ Until time.Time }

func (e *ErrRateLimited) Error() string {
	return fmt.Sprintf("rate limited until %s", e.Until.Format(time.RFC3339))
}

// AsRateLimited extracts an ErrRateLimited from an error chain.
func AsRateLimited(err error) (*ErrRateLimited, bool) {
	var rl *ErrRateLimited
	if errors.As(err, &rl) {
		return rl, true
	}
	return nil, false
}

// int64Array formats a Postgres bigint[] literal without pulling in pq.Array here.
type int64Array []int64

func (a int64Array) Value() (driver.Value, error) {
	b := make([]byte, 0, len(a)*8+2)
	b = append(b, '{')
	for i, v := range a {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, fmt.Sprint(v)...)
	}
	b = append(b, '}')
	return string(b), nil
}
