package stages

import (
	"context"
	"time"

	"github.com/mcpkg/mcpkg/internal/installgen"
	"github.com/mcpkg/mcpkg/internal/model"
	"github.com/mcpkg/mcpkg/internal/pipeline"
	"github.com/mcpkg/mcpkg/internal/queue"
	"github.com/mcpkg/mcpkg/internal/score"
)

// Render is hook 5: compute the deterministic score, build the install
// manifest that fills the page template, mark the tool verified, and schedule
// its next re-check. Pages are not generated files: one HTML template is
// filled from this data at request time.
type Render struct {
	D     *Deps
	Queue *queue.Queue // when set, schedules the next health re-check
}

// Name implements pipeline.Stage.
func (r *Render) Name() string { return "render" }

// Run implements pipeline.Stage.
func (r *Render) Run(ctx context.Context, tools []*model.Tool) []pipeline.Result {
	out := make([]pipeline.Result, len(tools))
	now := r.D.now()
	for i, t := range tools {
		if t.Summary == "" && t.Enrich == nil {
			// Skipped enrich (SkipHeavy path): use the deterministic fallback text.
			e := Fallback(t, BuildInput(t))
			e.CheckedAt = now
			t.Enrich, t.Summary, t.Categories = e, e.Summary, e.Categories
		}
		sc := score.Compute(t, now)
		t.Score = &sc
		verified := now
		t.VerifiedAt = &verified
		manifest := installgen.Build(t, r.D.Cfg.SiteURL, now)
		fields := map[string]any{
			"score": sc, "score_total": sc.Total, "grade": sc.Grade,
			"install": manifest, "verified_at": now,
		}
		if t.Enrich != nil && t.Summary != "" {
			fields["enrich"], fields["summary"], fields["categories"] = t.Enrich, t.Summary, t.Categories
		}
		if t.Status == "new" {
			fields["status"] = "active"
		}
		out[i] = pipeline.Result{Decision: pipeline.Continue, Fields: fields, Record: sc}
		if r.Queue != nil && r.D.Pipe.Recheck.HealthEveryDays > 0 && t.Status != "blocked" {
			_, _ = r.Queue.Enqueue(ctx, r.D.Pipe.Entry, t.ID, now.Add(time.Duration(r.D.Pipe.Recheck.HealthEveryDays)*24*time.Hour))
		}
	}
	return out
}
