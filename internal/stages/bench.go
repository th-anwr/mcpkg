package stages

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/mcpkg/mcpkg/internal/bench"
	"github.com/mcpkg/mcpkg/internal/model"
	"github.com/mcpkg/mcpkg/internal/pipeline"
	"github.com/mcpkg/mcpkg/internal/store"
)

// Bench is the benchmark stage (post-render, off the default chain). It only
// measures: results are appended to tool_benchmarks, and nothing here can
// change a tool's score, status, visibility or install data (gate B5).
type Bench struct {
	D        *Deps
	Runner   *bench.Runner
	Scanners []bench.Scanner // built from BENCH_SCANNERS by app.New
	Enabled  bool            // BENCH_ENABLED: false makes the stage a fast no-op
}

// Name implements pipeline.Stage.
func (s *Bench) Name() string { return "bench" }

// Run implements pipeline.Stage.
func (s *Bench) Run(ctx context.Context, tools []*model.Tool) []pipeline.Result {
	out := make([]pipeline.Result, len(tools))
	for i, t := range tools {
		out[i] = s.one(ctx, t)
	}
	return out
}

func (s *Bench) one(ctx context.Context, t *model.Tool) pipeline.Result {
	res := pipeline.Result{Decision: pipeline.Continue}
	if !s.Enabled || s.Runner == nil || len(s.Scanners) == 0 {
		return res
	}
	target := bench.Target{Slug: t.Slug, RepoURL: t.RepoURL, Packages: t.Packages, Remotes: t.Remotes}
	rows := s.Runner.RunTool(ctx, s.Scanners, target)
	benchRows := make([]store.BenchmarkRow, 0, len(rows))
	for _, r := range rows {
		if err := r.Valid(); err != nil {
			// B0: a malformed row becomes an error verdict with the reason in
			// raw, never a silently stored bad row.
			s.D.Log.Error("benchmark row invalid", "tool", t.Slug, "scanner", r.Scanner, "err", err)
			r = bench.Result{Scanner: r.Scanner, ScannerVersion: r.ScannerVersion, RanAt: r.RanAt,
				DurationMs: r.DurationMs, ExitCode: 1, Verdict: bench.VerdictError,
				Raw: rawMsg("schema error: " + err.Error())}
		}
		benchRows = append(benchRows, store.BenchmarkRow{
			Scanner: r.Scanner, ScannerVersion: r.ScannerVersion, ImageDigest: r.ImageDigest, RanAt: r.RanAt,
			DurationMs: r.DurationMs, ExitCode: r.ExitCode, Verdict: string(r.Verdict),
			SkipReason: r.SkipReason, Raw: r.Raw,
		})
	}
	if err := s.D.Store.SaveBenchmarks(ctx, t.ID, 0, benchRows); err != nil {
		res.Err = err
		return res
	}
	res.Notes = []string{"benchmarks: " + strconv.Itoa(len(benchRows)) + " scanners"}
	return res
}

func rawMsg(s string) []byte {
	b, _ := json.Marshal(map[string]any{"error": s})
	return b
}
