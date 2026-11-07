package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/mcpkg/mcpkg/internal/model"
)

// ScriptHook runs an external executable after a stage. Contract:
//
//	stdin : {"stage": "...", "decision": "...", "tool": {...}, "result": {...}}
//	stdout: {"decision": "continue|skip_heavy|stop", "notes": ["..."], "review": "", "data": {...}}
//
// Hooks can only tighten a decision. Write them in any language (see hooks/examples).
type ScriptHook struct {
	Config HookConfig
}

// HookResult is the parsed hook output.
type HookResult struct {
	Decision   Decision       `json:"decision"`
	Notes      []string       `json:"notes"`
	Review     string         `json:"review"`
	Data       map[string]any `json:"data"`
	DurationMs int            `json:"-"`
}

// hookInput is what a hook receives. The README is omitted to keep payloads small.
type hookInput struct {
	Stage    string      `json:"stage"`
	Decision Decision    `json:"decision"`
	Tool     *model.Tool `json:"tool"`
	Result   any         `json:"result"`
}

// NewHooks builds hooks from config.
func NewHooks(cfgs []HookConfig) []*ScriptHook {
	var out []*ScriptHook
	for _, c := range cfgs {
		if c.TimeoutSec <= 0 {
			c.TimeoutSec = 30
		}
		out = append(out, &ScriptHook{Config: c})
	}
	return out
}

// Run executes the hook. Failures follow on_error (continue by default).
func (h *ScriptHook) Run(ctx context.Context, t *model.Tool, stage string, res Result) HookResult {
	start := time.Now()
	in, _ := json.Marshal(hookInput{Stage: stage, Decision: res.Decision, Tool: t, Result: res.Fields[stage]})
	cctx, cancel := context.WithTimeout(ctx, time.Duration(h.Config.TimeoutSec)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, h.Config.Command[0], h.Config.Command[1:]...)
	cmd.Stdin = bytes.NewReader(in)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := HookResult{DurationMs: int(time.Since(start).Milliseconds())}
	fail := func(msg string) HookResult {
		out.Decision = Continue
		if h.Config.OnError == "stop" {
			out.Decision = Stop
		}
		out.Notes = []string{fmt.Sprintf("hook %s error: %s", h.Config.Name, msg)}
		return out
	}
	if err != nil {
		return fail(strings.TrimSpace(err.Error() + " " + tail(stderr.String(), 300)))
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		return fail("invalid JSON output: " + tail(stdout.String(), 200))
	}
	switch out.Decision {
	case Continue, SkipHeavy, Stop:
	case "":
		out.Decision = Continue
	default:
		return fail("unknown decision " + string(out.Decision))
	}
	out.DurationMs = int(time.Since(start).Milliseconds())
	return out
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
