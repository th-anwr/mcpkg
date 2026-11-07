// Package pipeline runs the gated stages (health -> security -> smoke -> enrich -> render)
// over the job queue, plus user-defined script hooks.
package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// StageConfig tunes one stage.
type StageConfig struct {
	Enabled     bool   `json:"enabled"`
	Concurrency int    `json:"concurrency"`
	Batch       int    `json:"batch"`
	TimeoutSec  int    `json:"timeout_sec"`
	LeaseSec    int    `json:"lease_sec"`
	MaxAttempts int    `json:"max_attempts"`
	Next        string `json:"next"`
}

// Timeout returns the per-batch timeout.
func (s StageConfig) Timeout() time.Duration {
	return time.Duration(max(s.TimeoutSec, 10)) * time.Second
}

// Lease returns the queue lease (must exceed the timeout).
func (s StageConfig) Lease() time.Duration {
	l := time.Duration(s.LeaseSec) * time.Second
	if l < s.Timeout()+30*time.Second {
		l = s.Timeout() + 30*time.Second
	}
	return l
}

// Gates are the knobs for skip/stop decisions between stages.
type Gates struct {
	SkipHeavyIfArchived     bool `json:"skip_heavy_if_archived"`
	SkipHeavyIfNoCommitDays int  `json:"skip_heavy_if_no_commit_days"` // 0 = off
	StopOnMalicious         bool `json:"stop_on_malicious"`
	ReviewOnHighFindings    bool `json:"review_on_high_findings"`
	SkipUnchangedWithinDays int  `json:"skip_unchanged_within_days"` // re-checks: skip heavy stages if version+commit unchanged
}

// Recheck controls the schedule after a tool is rendered.
type Recheck struct {
	HealthEveryDays int `json:"health_every_days"`
}

// HookConfig declares an external script hook.
type HookConfig struct {
	Name       string   `json:"name"`
	After      string   `json:"after"` // stage name
	Command    []string `json:"command"`
	TimeoutSec int      `json:"timeout_sec"`
	Enabled    bool     `json:"enabled"`
	OnError    string   `json:"on_error"` // continue (default) | stop
}

// Config is pipeline.json.
type Config struct {
	Entry   string                 `json:"entry"`
	Order   []string               `json:"order"`
	Stages  map[string]StageConfig `json:"stages"`
	Gates   Gates                  `json:"gates"`
	Recheck Recheck                `json:"recheck"`
	Hooks   []HookConfig           `json:"hooks"`
}

// Default returns the built-in configuration (used when pipeline.json is missing).
func Default() Config {
	return Config{
		Entry: "health",
		Order: []string{"health", "security", "smoke", "enrich", "render"},
		Stages: map[string]StageConfig{
			"health":   {Enabled: true, Concurrency: 2, Batch: 25, TimeoutSec: 180, MaxAttempts: 5, Next: "security"},
			"security": {Enabled: true, Concurrency: 2, Batch: 1, TimeoutSec: 300, MaxAttempts: 3, Next: "smoke"},
			"smoke":    {Enabled: true, Concurrency: 2, Batch: 1, TimeoutSec: 300, MaxAttempts: 2, Next: "enrich"},
			"enrich":   {Enabled: true, Concurrency: 4, Batch: 1, TimeoutSec: 120, MaxAttempts: 3, Next: "render"},
			"render":   {Enabled: true, Concurrency: 2, Batch: 25, TimeoutSec: 60, MaxAttempts: 3, Next: ""},
			// Benchmarks run off the default chain: `mcpkg bench` enqueues and
			// `mcpkg worker -stages bench` drains. Off until BENCH_ENABLED=1.
			"bench": {Enabled: true, Concurrency: 1, Batch: 1, TimeoutSec: 360, MaxAttempts: 2, Next: ""},
		},
		Gates:   Gates{SkipHeavyIfArchived: true, StopOnMalicious: true, ReviewOnHighFindings: true, SkipUnchangedWithinDays: 30},
		Recheck: Recheck{HealthEveryDays: 7},
	}
}

// Load reads pipeline.json, filling unspecified values from Default().
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	var file struct {
		Config
		Gates *Gates `json:"gates"` // pointer: keep safe defaults when the file omits gates
	}
	if err := json.Unmarshal(b, &file); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if file.Entry != "" {
		cfg.Entry = file.Entry
	}
	if len(file.Order) > 0 {
		cfg.Order = file.Order
	}
	for name, sc := range file.Stages {
		cfg.Stages[name] = sc
	}
	if file.Recheck.HealthEveryDays > 0 {
		cfg.Recheck = file.Recheck
	}
	if file.Gates != nil {
		cfg.Gates = *file.Gates
	}
	cfg.Hooks = file.Hooks
	return cfg, cfg.Validate()
}

// Validate checks the stage graph.
func (c Config) Validate() error {
	if _, ok := c.Stages[c.Entry]; !ok {
		return fmt.Errorf("entry stage %q is not configured", c.Entry)
	}
	for name, s := range c.Stages {
		if s.Next != "" {
			if _, ok := c.Stages[s.Next]; !ok {
				return fmt.Errorf("stage %q: next stage %q is not configured", name, s.Next)
			}
		}
		if s.Concurrency < 0 || s.Batch < 0 {
			return fmt.Errorf("stage %q: negative concurrency/batch", name)
		}
	}
	for _, h := range c.Hooks {
		if h.Enabled && len(h.Command) == 0 {
			return fmt.Errorf("hook %q has no command", h.Name)
		}
		if _, ok := c.Stages[h.After]; h.Enabled && !ok {
			return fmt.Errorf("hook %q runs after unknown stage %q", h.Name, h.After)
		}
	}
	return nil
}
