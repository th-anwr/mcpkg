package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/mcpkg/mcpkg/internal/model"
	"github.com/mcpkg/mcpkg/internal/queue"
	"github.com/mcpkg/mcpkg/internal/store"
)

// Decision is a gate outcome.
type Decision string

const (
	// Continue to the configured next stage.
	Continue Decision = "continue"
	// SkipHeavy jumps straight to "render" (skips security/smoke/enrich).
	SkipHeavy Decision = "skip_heavy"
	// Stop ends the pipeline for this tool (e.g. malicious, missing).
	Stop Decision = "stop"
)

// RenderStage is where SkipHeavy jumps to.
const RenderStage = "render"

// Result is what a stage returns for one tool.
type Result struct {
	Decision Decision
	Fields   map[string]any // tool columns to update (health, smoke, stars, status, ...)
	Record   any            // stored in check_runs.result (defaults to the main field)
	Notes    []string
	Review   string // non-empty: open a human review item with this reason
	Err      error  // transient failure: the job is retried with backoff
}

// Stage processes a batch of tools. len(results) must equal len(tools).
type Stage interface {
	Name() string
	Run(ctx context.Context, tools []*model.Tool) []Result
}

// Runner executes stages from the queue.
type Runner struct {
	Store    *store.Store
	Queue    *queue.Queue
	Config   Config
	Stages   map[string]Stage
	Hooks    []*ScriptHook
	WorkerID string
	Log      *slog.Logger
}

// NextStage applies the gate decision to the stage graph.
func (r *Runner) NextStage(current string, d Decision) string {
	switch d {
	case Stop:
		return ""
	case SkipHeavy:
		if current == RenderStage {
			return ""
		}
		if _, ok := r.Config.Stages[RenderStage]; ok {
			return RenderStage
		}
		return ""
	default:
		next := r.Config.Stages[current].Next
		// Skip disabled stages.
		for next != "" && !r.Config.Stages[next].Enabled {
			next = r.Config.Stages[next].Next
		}
		return next
	}
}

// Work runs worker loops for the given stages until ctx is cancelled.
func (r *Runner) Work(ctx context.Context, stageNames []string) error {
	var wg sync.WaitGroup
	started := 0
	for _, name := range stageNames {
		sc, ok := r.Config.Stages[name]
		if !ok || !sc.Enabled {
			r.Log.Info("stage disabled or unknown, skipping", "stage", name)
			continue
		}
		st, ok := r.Stages[name]
		if !ok {
			return fmt.Errorf("no implementation for stage %q", name)
		}
		conc := max(sc.Concurrency, 1)
		for i := 0; i < conc; i++ {
			wg.Add(1)
			started++
			go func(slot int) {
				defer wg.Done()
				r.loop(ctx, st, sc, slot)
			}(i)
		}
	}
	if started == 0 {
		return errors.New("no stages to run")
	}
	// Lease reaper: returns jobs of crashed/interrupted workers to the queue.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := r.Queue.ReapExpired(context.Background()); err == nil && n > 0 {
					r.Log.Warn("requeued expired leases", "jobs", n)
				}
			}
		}
	}()
	wg.Wait()
	return nil
}

func (r *Runner) loop(ctx context.Context, st Stage, sc StageConfig, slot int) {
	worker := fmt.Sprintf("%s/%s/%d", r.WorkerID, st.Name(), slot)
	idle := time.Second
	for ctx.Err() == nil {
		jobs, err := r.Queue.Claim(ctx, st.Name(), max(sc.Batch, 1), worker, sc.Lease())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.Log.Error("claim failed", "stage", st.Name(), "err", err)
			sleep(ctx, 5*time.Second)
			continue
		}
		if len(jobs) == 0 {
			sleep(ctx, idle+time.Duration(rand.IntN(1000))*time.Millisecond)
			if idle < 15*time.Second {
				idle *= 2
			}
			continue
		}
		idle = time.Second
		r.process(ctx, st, sc, jobs, worker)
	}
}

// process runs one claimed batch end to end.
func (r *Runner) process(ctx context.Context, st Stage, sc StageConfig, jobs []queue.Job, worker string) {
	ids := make([]int64, len(jobs))
	byTool := map[int64]queue.Job{}
	for i, j := range jobs {
		ids[i] = j.ToolID
		byTool[j.ToolID] = j
	}
	tools, err := r.Store.GetTools(ctx, ids)
	if err != nil {
		for _, j := range jobs {
			_ = r.Queue.Fail(context.Background(), j, err)
		}
		return
	}
	found := map[int64]bool{}
	for _, t := range tools {
		found[t.ID] = true
	}
	for _, j := range jobs { // tool deleted/merged since enqueue
		if !found[j.ToolID] {
			_ = r.Queue.Complete(context.Background(), j.ID)
		}
	}
	if len(tools) == 0 {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, sc.Timeout())
	start := time.Now()
	results := safeRun(sctx, st, tools)
	cancel()
	elapsed := time.Since(start)
	for i, t := range tools {
		j := byTool[t.ID]
		res := results[i]
		if err := r.Finish(context.Background(), st.Name(), t, res, int(elapsed.Milliseconds())/len(tools), worker); err != nil {
			if rl, ok := queue.AsRateLimited(err); ok {
				_ = r.Queue.RetryAfter(context.Background(), j, time.Until(rl.Until)+time.Minute, err.Error())
				continue
			}
			r.Log.Warn("stage failed", "stage", st.Name(), "tool", t.Slug, "attempt", j.Attempts, "err", err)
			_ = r.Queue.Fail(context.Background(), j, err)
			continue
		}
		_ = r.Queue.Complete(context.Background(), j.ID)
	}
}

// Finish persists a stage result, runs hooks, and enqueues the next stage.
func (r *Runner) Finish(ctx context.Context, stage string, t *model.Tool, res Result, durationMs int, worker string) error {
	_, err := r.finish(ctx, stage, t, res, durationMs, worker, true)
	return err
}

// finish returns the final decision after hooks; enqueue=false is used by RunSync.
func (r *Runner) finish(ctx context.Context, stage string, t *model.Tool, res Result, durationMs int, worker string, enqueue bool) (Decision, error) {
	if res.Err != nil {
		return "", res.Err
	}
	if res.Decision == "" {
		res.Decision = Continue
	}
	// Script hooks after this stage may tighten (never loosen) the decision.
	for _, h := range r.Hooks {
		if !h.Config.Enabled || h.Config.After != stage {
			continue
		}
		hr := h.Run(ctx, t, stage, res)
		res.Decision = Strictest(res.Decision, hr.Decision)
		res.Notes = append(res.Notes, hr.Notes...)
		if hr.Review != "" && res.Review == "" {
			res.Review = hr.Review
		}
		_ = r.Store.SaveStage(ctx, store.StageRecord{
			ToolID: t.ID, Stage: "hook:" + h.Config.Name, Decision: string(hr.Decision), Notes: hr.Notes,
			Result: hr.Data, DurationMs: hr.DurationMs, Worker: worker,
		})
	}
	if res.Decision == Stop {
		if res.Fields == nil {
			res.Fields = map[string]any{}
		}
		if _, ok := res.Fields["visibility"]; !ok && res.Fields["status"] == "blocked" {
			res.Fields["visibility"] = "hidden"
		}
	}
	record := res.Record
	if record == nil {
		record = res.Fields[stage]
	}
	if err := r.Store.SaveStage(ctx, store.StageRecord{
		ToolID: t.ID, Stage: stage, Decision: string(res.Decision), Notes: res.Notes,
		Result: record, Fields: res.Fields, DurationMs: durationMs, Worker: worker,
	}); err != nil {
		return "", err
	}
	if res.Review != "" {
		_ = r.Store.AddReview(ctx, t.ID, res.Review, map[string]any{"stage": stage, "notes": res.Notes})
	}
	if enqueue {
		if next := r.NextStage(stage, res.Decision); next != "" {
			if _, err := r.Queue.EnqueuePriority(ctx, next, t.ID, time.Time{}, Priority(t, res.Fields)); err != nil {
				return res.Decision, err
			}
		}
	}
	return res.Decision, nil
}

// RunSync pushes one tool through every stage in-process (debugging, `mcpkg run`).
// It persists results exactly like workers do, but drives the stage graph itself.
func (r *Runner) RunSync(ctx context.Context, toolID int64, from string, log func(stage string, res Result)) error {
	stage := from
	if stage == "" {
		stage = r.Config.Entry
	}
	for stage != "" {
		st, ok := r.Stages[stage]
		if !ok {
			return fmt.Errorf("no implementation for stage %q", stage)
		}
		t, err := r.Store.GetTool(ctx, toolID)
		if err != nil {
			return err
		}
		sc := r.Config.Stages[stage]
		sctx, cancel := context.WithTimeout(ctx, sc.Timeout())
		start := time.Now()
		res := safeRun(sctx, st, []*model.Tool{t})[0]
		cancel()
		if log != nil {
			log(stage, res)
		}
		if res.Err != nil {
			return fmt.Errorf("%s: %w", stage, res.Err)
		}
		decision, err := r.finish(ctx, stage, t, res, int(time.Since(start).Milliseconds()), r.WorkerID+"/sync", false)
		if err != nil {
			return err
		}
		stage = r.NextStage(stage, decision)
	}
	return nil
}

// Priority orders the heavy stages: popular tools (stars, downloads) are verified first.
func Priority(t *model.Tool, fields map[string]any) int {
	stars, dl := t.Stars, t.WeeklyDownloads
	if v, ok := fields["stars"].(int); ok && v > stars {
		stars = v
	}
	if v, ok := fields["weekly_downloads"].(int64); ok && v > dl {
		dl = v
	}
	p := int64(stars) + dl/100
	if p > 1_000_000 {
		p = 1_000_000
	}
	return int(p)
}

// Strictest combines two decisions: stop > skip_heavy > continue.
func Strictest(a, b Decision) Decision {
	rank := map[Decision]int{Continue: 0, "": 0, SkipHeavy: 1, Stop: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// safeRun protects workers from panics inside a stage.
func safeRun(ctx context.Context, st Stage, tools []*model.Tool) (out []Result) {
	defer func() {
		if p := recover(); p != nil {
			out = make([]Result, len(tools))
			for i := range out {
				out[i] = Result{Err: fmt.Errorf("panic in stage %s: %v", st.Name(), p)}
			}
		}
	}()
	out = st.Run(ctx, tools)
	if len(out) != len(tools) {
		res := make([]Result, len(tools))
		for i := range res {
			if i < len(out) {
				res[i] = out[i]
			} else {
				res[i] = Result{Err: fmt.Errorf("stage %s returned %d results for %d tools", st.Name(), len(out), len(tools))}
			}
		}
		out = res
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
