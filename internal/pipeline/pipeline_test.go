package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mcpkg/mcpkg/internal/model"
)

func TestNextStage(t *testing.T) {
	r := &Runner{Config: Default()}
	cases := []struct {
		cur  string
		d    Decision
		want string
	}{
		{"health", Continue, "security"},
		{"health", SkipHeavy, "render"},
		{"health", Stop, ""},
		{"smoke", Continue, "enrich"},
		{"render", Continue, ""},
		{"render", SkipHeavy, ""},
	}
	for _, c := range cases {
		if got := r.NextStage(c.cur, c.d); got != c.want {
			t.Errorf("NextStage(%s,%s)=%q want %q", c.cur, c.d, got, c.want)
		}
	}
	// Disabled stages are skipped over.
	cfg := Default()
	sc := cfg.Stages["security"]
	sc.Enabled = false
	cfg.Stages["security"] = sc
	r = &Runner{Config: cfg}
	if got := r.NextStage("health", Continue); got != "smoke" {
		t.Errorf("disabled security should be skipped, got %q", got)
	}
}

func TestStrictest(t *testing.T) {
	if Strictest(Continue, Stop) != Stop || Strictest(SkipHeavy, Continue) != SkipHeavy || Strictest(Stop, SkipHeavy) != Stop {
		t.Fatal("strictest ordering broken")
	}
}

func TestScriptHook(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "hook.sh")
	_ = os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\necho '{\"decision\":\"skip_heavy\",\"notes\":[\"no license\"],\"review\":\"license\"}'\n"), 0o755)
	h := NewHooks([]HookConfig{{Name: "lic", After: "health", Command: []string{script}, Enabled: true}})[0]
	hr := h.Run(context.Background(), &model.Tool{Name: "x"}, "health", Result{Decision: Continue})
	if hr.Decision != SkipHeavy || len(hr.Notes) != 1 || hr.Review != "license" {
		t.Errorf("hook result = %+v", hr)
	}
	bad := filepath.Join(dir, "bad.sh")
	_ = os.WriteFile(bad, []byte("#!/bin/sh\necho not-json\n"), 0o755)
	h = NewHooks([]HookConfig{{Name: "bad", After: "health", Command: []string{bad}, Enabled: true, OnError: "stop"}})[0]
	if hr := h.Run(context.Background(), &model.Tool{}, "health", Result{}); hr.Decision != Stop {
		t.Errorf("on_error=stop should stop, got %+v", hr)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pipeline.json")
	_ = os.WriteFile(p, []byte(`{"stages":{"smoke":{"enabled":true,"concurrency":8,"batch":1,"timeout_sec":200,"next":"enrich"}},"gates":{"skip_heavy_if_archived":true},"hooks":[{"name":"x","after":"nope","command":["true"],"enabled":true}]}`), 0o644)
	if _, err := Load(p); err == nil {
		t.Error("hook after unknown stage should fail validation")
	}
	_ = os.WriteFile(p, []byte(`{"stages":{"smoke":{"enabled":true,"concurrency":8,"batch":1,"timeout_sec":200,"next":"enrich"}},"gates":{"skip_heavy_if_archived":true}}`), 0o644)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Stages["smoke"].Concurrency != 8 || cfg.Stages["health"].Next != "security" {
		t.Errorf("merge with defaults failed: %+v", cfg.Stages)
	}
	if missing, err := Load(filepath.Join(dir, "none.json")); err != nil || missing.Entry != "health" {
		t.Error("missing file should yield defaults")
	}
}
