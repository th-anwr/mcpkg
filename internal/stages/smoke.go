package stages

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mcpkg/mcpkg/internal/launch"
	"github.com/mcpkg/mcpkg/internal/mcpclient"
	"github.com/mcpkg/mcpkg/internal/model"
	"github.com/mcpkg/mcpkg/internal/pipeline"
	"github.com/mcpkg/mcpkg/internal/sandbox"
	"github.com/mcpkg/mcpkg/internal/scan"
)

// Smoke is hook 3: install in a sandbox, start with no network, run the MCP
// handshake (initialize + tools/list). Pass/fail is decided by protocol
// responses and exit codes, never by an LLM.
type Smoke struct {
	D              *Deps
	Runner         sandbox.Runner // nil when SANDBOX=off
	InstallTimeout time.Duration
	StartTimeout   time.Duration
	AllowPrivate   bool // tests only: allow remote checks against localhost
}

// Name implements pipeline.Stage.
func (s *Smoke) Name() string { return "smoke" }

// Run implements pipeline.Stage.
func (s *Smoke) Run(ctx context.Context, tools []*model.Tool) []pipeline.Result {
	out := make([]pipeline.Result, len(tools))
	for i, t := range tools {
		out[i] = s.one(ctx, t)
	}
	return out
}

func (s *Smoke) one(ctx context.Context, t *model.Tool) pipeline.Result {
	sm := &model.Smoke{CheckedAt: s.D.now(), Version: t.Version}
	pathPH := "/tmp/mcpkg-smoke"
	jobID := fmt.Sprintf("%d-%d", t.ID, time.Now().UnixNano()%1e9)
	if s.Runner != nil {
		pathPH = s.Runner.PathPlaceholder(sandbox.Job{ID: jobID})
	}
	plan := launch.Choose(t, pathPH)
	sm.Method = plan.Kind
	fields := map[string]any{}
	var notes []string

	switch plan.Kind {
	case "none":
		sm.Status, sm.SkipReason = "skipped", plan.SkipReason
	case "remote":
		sm.Command = plan.Remote.URL
		hctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		client := mcpclient.SafeHTTPClient(40*time.Second, s.AllowPrivate)
		start := time.Now()
		info, err := mcpclient.HandshakeHTTPLog(hctx, client, plan.Remote.URL, plan.Remote.Type, nil,
			s.D.Log.With("component", "mcpclient", "tool", t.Slug))
		cancel()
		sm.StartupMs = time.Since(start).Milliseconds()
		s.applyInfo(sm, info)
		sm.Status, sm.CredentialHint = Classify(info, err, "")
		if err != nil {
			sm.Error = truncate(err.Error(), 500)
		}
	default:
		if s.Runner == nil {
			sm.Status, sm.SkipReason = "skipped", "sandbox disabled on this worker"
			break
		}
		sm.Sandbox = s.Runner.Name()
		sm.Package = plan.Package.Identifier
		job := sandbox.Job{ID: jobID, Kind: plan.Kind, Package: plan.Package.Identifier, Version: plan.Package.Version,
			PreferBin: plan.PreferBin, EntryPoints: plan.EntryPoints, Args: plan.Args, Env: plan.Env}
		defer s.Runner.Cleanup(job)
		ictx, cancel := context.WithTimeout(ctx, s.installTimeout())
		start := time.Now()
		bin, log, err := s.Runner.Install(ictx, job)
		cancel()
		sm.InstallMs = time.Since(start).Milliseconds()
		if err != nil {
			if errors.Is(err, sandbox.ErrInfra) {
				return pipeline.Result{Err: err} // retry later; not the tool's fault
			}
			sm.Status, sm.Error, sm.LogTail = "install_failed", truncate(err.Error(), 500), redact(tailString(log, 2000), job.Env)
			break
		}
		sm.Bin = bin
		proc, err := s.Runner.Start(ctx, job, bin)
		if err != nil {
			if errors.Is(err, sandbox.ErrInfra) {
				return pipeline.Result{Err: err}
			}
			sm.Status, sm.Error = "broken", truncate(err.Error(), 500)
			break
		}
		sm.Command = proc.Display
		hctx, hcancel := context.WithTimeout(ctx, s.startTimeout())
		start = time.Now()
		info, herr := mcpclient.HandshakeStdioLog(hctx, proc.Stdin, proc.Stdout,
			s.D.Log.With("component", "mcpclient", "tool", t.Slug))
		hcancel()
		sm.StartupMs = time.Since(start).Milliseconds()
		proc.Close()
		stderr := proc.Stderr()
		s.applyInfo(sm, info)
		sm.Status, sm.CredentialHint = Classify(info, herr, stderr)
		if herr != nil {
			sm.Error = truncate(herr.Error(), 500)
		}
		if sm.Status != "works" {
			sm.LogTail = redact(tailString(stderr, 2000), job.Env)
		}
		// Remember the resolved executable so install instructions are exact.
		if bin != "" && plan.PackageIdx < len(t.Packages) && t.Packages[plan.PackageIdx].Bin != bin {
			pkgs := append([]model.Package(nil), t.Packages...)
			pkgs[plan.PackageIdx].Bin = bin
			fields["packages"] = pkgs
		}
	}
	for _, tool := range sm.Tools {
		sm.DescriptionFindings = append(sm.DescriptionFindings, scan.Text("tool:"+tool.Name, tool.Name+"\n"+tool.Title+"\n"+tool.Description)...)
	}
	fields["smoke"] = sm
	fields["smoke_status"] = sm.Status
	res := pipeline.Result{Decision: pipeline.Continue, Fields: fields, Notes: append(notes, "smoke: "+sm.Status)}
	if len(sm.DescriptionFindings) > 0 {
		res.Review = "tool-description-findings"
		res.Notes = append(res.Notes, fmt.Sprintf("%d suspicious tool descriptions", len(sm.DescriptionFindings)))
	}
	return res
}

func (s *Smoke) applyInfo(sm *model.Smoke, info *mcpclient.Info) {
	if info == nil {
		return
	}
	sm.ProtocolVersion, sm.ServerName, sm.ServerVersion = info.ProtocolVersion, info.ServerName, info.ServerVersion
	sm.Capabilities, sm.Tools = info.Capabilities, info.Tools
	sm.PromptsCount, sm.ResourcesCount, sm.StdoutPollution = info.PromptsCount, info.ResourcesCount, info.StdoutPollution
}

func (s *Smoke) installTimeout() time.Duration {
	if s.InstallTimeout > 0 {
		return s.InstallTimeout
	}
	return 4 * time.Minute
}

func (s *Smoke) startTimeout() time.Duration {
	if s.StartTimeout > 0 {
		return s.StartTimeout
	}
	return 45 * time.Second
}

var credRe = regexp.MustCompile(`(?i)(api[_ -]?key|access[_ -]?token|auth[_ -]?token|bearer|secret[_ -]?key|client[_ -]?secret|credential|unauthori[sz]ed|\b401\b|\b403\b|forbidden|invalid[_ -](api[_ -]?)?(key|token)|missing[^\n]{0,40}\b(env|environment|variable|key|token)\b|(environment variable|env var)[^\n]{0,40}(not set|is required|required|must be set|missing)|\bis not set\b|must be set|not configured|please (set|provide)|sign[ -]?in|log ?in required|` + launch.Placeholder + `)`)

// Classify maps a handshake outcome to a smoke status. Deterministic by design.
func Classify(info *mcpclient.Info, err error, logs string) (status string, credentialHint bool) {
	text := logs
	if err != nil {
		text += "\n" + err.Error()
	}
	if info != nil {
		text += "\n" + info.ToolsError
	}
	credentialHint = credRe.MatchString(text)
	switch {
	case err == nil && info != nil:
		if info.ToolsError != "" && credentialHint && len(info.Tools) == 0 {
			return "needs_credentials", true
		}
		return "works", credentialHint
	case errors.Is(err, mcpclient.ErrAuthRequired):
		return "auth_required", true
	case credentialHint:
		return "needs_credentials", true
	default:
		return "broken", false
	}
}

// redact removes placeholder/env values from logs before storing them.
func redact(s string, env map[string]string) string {
	for _, v := range env {
		if len(v) >= 6 && v != launch.Placeholder {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}
