// Package score computes the deterministic trust score (see docs/SCORING.md).
// Same inputs always give the same score; no LLM is involved.
package score

import (
	"fmt"
	"math"
	"time"

	"github.com/mcpkg/mcpkg/internal/model"
)

// Version identifies the formula (shown on pages; bump when weights change).
const Version = "2026.10-1"

// Weights of the four parts (sum = 100).
var Weights = map[string]int{"maintenance": 30, "security": 30, "works": 25, "community": 15}

// Compute scores a tool as of now.
func Compute(t *model.Tool, now time.Time) model.Score {
	s := model.Score{Weights: Weights, Version: Version, ComputedAt: now}
	var reasons []string
	s.Maintenance, reasons = maintenance(t, now, reasons)
	s.Security, reasons = security(t, reasons)
	s.Works, reasons = works(t, reasons)
	s.Community, reasons = community(t, reasons)
	total := float64(s.Maintenance*Weights["maintenance"]+s.Security*Weights["security"]+
		s.Works*Weights["works"]+s.Community*Weights["community"]) / 100.0
	capTo := func(limit int, why string) {
		if total > float64(limit) {
			total = float64(limit)
		}
		s.Caps = append(s.Caps, why)
	}
	if t.Status == "blocked" || (t.Security != nil && t.Security.Blocked) {
		total = 0
		s.Caps = append(s.Caps, "blocked")
	}
	if t.Health != nil && (t.Health.Archived || t.Health.Disabled) {
		capTo(40, "archived repository (max 40)")
	}
	if t.SmokeStatus == "broken" || t.SmokeStatus == "install_failed" {
		capTo(50, "fails to start (max 50)")
	}
	if t.Security != nil && t.Security.NeedsReview {
		capTo(60, "high-severity findings pending review (max 60)")
	}
	if t.Smoke != nil && len(t.Smoke.DescriptionFindings) > 0 {
		capTo(40, "suspicious tool descriptions (max 40)")
	}
	if t.Status == "deprecated" {
		capTo(30, "deprecated in the MCP Registry (max 30)")
	}
	s.Total = int(math.Round(total))
	s.Grade = Grade(s.Total)
	s.Reasons = reasons
	return s
}

// Grade maps a total to A-F.
func Grade(total int) string {
	switch {
	case total >= 85:
		return "A"
	case total >= 70:
		return "B"
	case total >= 55:
		return "C"
	case total >= 40:
		return "D"
	}
	return "F"
}

func maintenance(t *model.Tool, now time.Time, reasons []string) (int, []string) {
	h := t.Health
	last := t.LastCommitAt
	if h != nil && h.LastCommitAt != nil {
		last = h.LastCommitAt
	}
	if last == nil && t.PublishedAt != nil {
		last = t.PublishedAt
	}
	age := 30.0 // unknown
	if last != nil {
		days := now.Sub(*last).Hours() / 24
		switch {
		case days <= 30:
			age = 100
		case days <= 90:
			age = 85
		case days <= 180:
			age = 65
		case days <= 365:
			age = 40
		default:
			age = 15
		}
		reasons = append(reasons, fmt.Sprintf("last activity %d days ago", int(days)))
	}
	release := 30.0
	if h != nil && h.LastReleaseAt != nil {
		if now.Sub(*h.LastReleaseAt) < 180*24*time.Hour {
			release = 100
		} else {
			release = 60
		}
	} else if t.PublishedAt != nil && now.Sub(*t.PublishedAt) < 180*24*time.Hour {
		release = 90 // recent npm/PyPI publish counts as a release
	} else if t.PublishedAt != nil {
		release = 55
	}
	prs := 70.0
	if h != nil {
		closed := h.MergedPRs + h.ClosedPRs
		if closed+h.OpenPRs > 0 {
			prs = 100 * float64(h.MergedPRs+h.ClosedPRs/2) / float64(closed+h.OpenPRs)
		}
		if h.OldestOpenPRAt != nil && now.Sub(*h.OldestOpenPRAt) > 180*24*time.Hour {
			prs -= 20
			reasons = append(reasons, "pull requests left open for 6+ months")
		}
		if h.OpenPRs > 0 {
			reasons = append(reasons, fmt.Sprintf("%d open pull requests", h.OpenPRs))
		}
	}
	m := age*0.7 + release*0.15 + clamp(prs, 0, 100)*0.15
	if h != nil && (h.Archived || h.Disabled) {
		m = math.Min(m, 10)
		reasons = append(reasons, "repository archived")
	}
	return int(math.Round(clamp(m, 0, 100))), reasons
}

func security(t *model.Tool, reasons []string) (int, []string) {
	sec := t.Security
	if sec == nil || sec.Scanned == "none" || sec.Scanned == "" {
		return 50, append(reasons, "not scanned")
	}
	if sec.Blocked {
		return 0, append(reasons, "blocked: "+sec.BlockReason)
	}
	v := 100.0
	penalty := map[string]float64{"critical": 40, "high": 25, "medium": 8, "low": 2}
	for _, f := range sec.Findings {
		v -= penalty[f.Severity]
	}
	vulns := float64(sec.Vulns.Critical)*30 + float64(sec.Vulns.High)*15 + float64(sec.Vulns.Medium)*3
	v -= math.Min(vulns, 50)
	if sec.Vulns.Critical+sec.Vulns.High > 0 {
		reasons = append(reasons, fmt.Sprintf("%d critical/high dependency vulnerabilities", sec.Vulns.Critical+sec.Vulns.High))
	}
	if t.Smoke != nil {
		for range t.Smoke.DescriptionFindings {
			v -= 40
		}
	}
	v = clamp(v, 0, 100)
	if sec.Scorecard != nil {
		v = 0.75*v + 0.25*(*sec.Scorecard*10)
		reasons = append(reasons, fmt.Sprintf("OpenSSF Scorecard %.1f/10", *sec.Scorecard))
	}
	if n := sec.Counts["high"] + sec.Counts["critical"]; n > 0 {
		reasons = append(reasons, fmt.Sprintf("%d high-severity static findings", n))
	}
	return int(math.Round(clamp(v, 0, 100))), reasons
}

func works(t *model.Tool, reasons []string) (int, []string) {
	sm := t.Smoke
	if sm == nil {
		return 40, append(reasons, "not smoke-tested yet")
	}
	v := map[string]float64{
		"works": 100, "needs_credentials": 70, "auth_required": 75, "broken": 10, "install_failed": 0, "skipped": 40,
	}[sm.Status]
	if sm.Status == "works" {
		if len(sm.Tools) == 0 && sm.PromptsCount == 0 && sm.ResourcesCount == 0 {
			v -= 30
			reasons = append(reasons, "starts but exposes no tools")
		}
		if sm.StdoutPollution {
			v -= 10
			reasons = append(reasons, "prints logs to stdout (breaks some clients)")
		}
		if sm.StartupMs > 20000 {
			v -= 10
			reasons = append(reasons, "slow startup")
		}
	}
	reasons = append(reasons, "smoke test: "+sm.Status)
	return int(math.Round(clamp(v, 0, 100))), reasons
}

func community(t *model.Tool, reasons []string) (int, []string) {
	stars := t.Stars
	contributors := 0
	verified := false
	if t.Health != nil {
		if t.Health.Stars > stars {
			stars = t.Health.Stars
		}
		contributors = t.Health.Contributors
		verified = t.Health.OwnerVerified
	}
	starScore := 100 * math.Log10(float64(stars)+1) / math.Log10(5000)
	dlScore := 100 * math.Log10(float64(t.WeeklyDownloads)+1) / math.Log10(100000)
	contrib := 40.0
	switch {
	case contributors >= 10:
		contrib = 100
	case contributors >= 3:
		contrib = 70
	case contributors == 0:
		contrib = 30
	}
	v := math.Max(clamp(starScore, 0, 100), clamp(dlScore, 0, 100))*0.7 + contrib*0.3
	if verified || t.RegistryStatus() == "active" {
		v += 10 // verified org on GitHub, or namespace-verified publisher in the MCP Registry
	}
	if stars > 0 {
		reasons = append(reasons, fmt.Sprintf("%d stars", stars))
	}
	return int(math.Round(clamp(v, 0, 100))), reasons
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }
