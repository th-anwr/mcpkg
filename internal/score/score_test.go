package score

import (
	"testing"
	"time"

	"github.com/mcpkg/mcpkg/internal/model"
)

var now = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

func ago(days int) *time.Time { t := now.AddDate(0, 0, -days); return &t }

func healthyTool() *model.Tool {
	sc := 8.0
	return &model.Tool{
		Status: "active", SmokeStatus: "works", Stars: 2400, WeeklyDownloads: 30000,
		Health:   &model.Health{RepoFound: true, LastCommitAt: ago(5), LastReleaseAt: ago(20), MergedPRs: 80, ClosedPRs: 10, OpenPRs: 4, Contributors: 25, Stars: 2400},
		Security: &model.Security{Scanned: "npm-tarball", Scorecard: &sc, Counts: map[string]int{}},
		Smoke:    &model.Smoke{Status: "works", Tools: []model.ToolInfo{{Name: "a"}}, StartupMs: 900},
	}
}

func TestHealthyToolGetsA(t *testing.T) {
	s := Compute(healthyTool(), now)
	if s.Grade != "A" || s.Total < 85 {
		t.Fatalf("healthy tool: %+v", s)
	}
	if len(s.Caps) != 0 {
		t.Fatalf("unexpected caps: %v", s.Caps)
	}
}

func TestDeterministic(t *testing.T) {
	a, b := Compute(healthyTool(), now), Compute(healthyTool(), now)
	if a.Total != b.Total || a.Maintenance != b.Maintenance {
		t.Fatal("score must be deterministic")
	}
}

func TestCaps(t *testing.T) {
	tl := healthyTool()
	tl.Health.Archived = true
	if s := Compute(tl, now); s.Total > 40 {
		t.Errorf("archived must cap at 40, got %d", s.Total)
	}
	tl = healthyTool()
	tl.SmokeStatus, tl.Smoke.Status = "broken", "broken"
	if s := Compute(tl, now); s.Total > 50 {
		t.Errorf("broken must cap at 50, got %d", s.Total)
	}
	tl = healthyTool()
	tl.Security.Blocked = true
	if s := Compute(tl, now); s.Total != 0 || s.Grade != "F" {
		t.Errorf("blocked must be 0/F, got %d/%s", s.Total, s.Grade)
	}
	tl = healthyTool()
	tl.Smoke.DescriptionFindings = []model.Finding{{Rule: "prompt-injection-text", Severity: "high"}}
	if s := Compute(tl, now); s.Total > 40 {
		t.Errorf("tool poisoning must cap at 40, got %d", s.Total)
	}
}

func TestNeedsKeyIsNotPunishedLikeBroken(t *testing.T) {
	a := healthyTool()
	a.SmokeStatus, a.Smoke.Status = "needs_credentials", "needs_credentials"
	b := healthyTool()
	b.SmokeStatus, b.Smoke.Status = "broken", "broken"
	if Compute(a, now).Total <= Compute(b, now).Total+10 {
		t.Error("needs_credentials should score clearly above broken")
	}
}

func TestStaleRepoScoresLowerMaintenance(t *testing.T) {
	fresh := healthyTool()
	stale := healthyTool()
	stale.Health.LastCommitAt = ago(500)
	stale.Health.LastReleaseAt = ago(600)
	if Compute(stale, now).Maintenance >= Compute(fresh, now).Maintenance {
		t.Error("stale repo should have lower maintenance score")
	}
}

func TestGrade(t *testing.T) {
	for total, want := range map[int]string{100: "A", 85: "A", 84: "B", 70: "B", 55: "C", 40: "D", 39: "F", 0: "F"} {
		if g := Grade(total); g != want {
			t.Errorf("Grade(%d)=%s want %s", total, g, want)
		}
	}
}
