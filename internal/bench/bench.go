// Package bench runs the benchmark scanners that feed the Benchmarks tab and
// the deploy gate (docs/BENCH_GATE.md). It is deliberately free of decisions:
// scanners measure, they never change a tool's score, status, visibility or
// install data. Every result carries full provenance (scanner, version, image
// digest, run time) so the page and the gate report can show raw numbers.
package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mcpkg/mcpkg/internal/httpx"
	"github.com/mcpkg/mcpkg/internal/mcpclient"
	"github.com/mcpkg/mcpkg/internal/model"
)

// Verdict is the terminal state of one scanner for one tool.
type Verdict string

// Verdict enum. A verdict is a measurement, never a moderation decision.
const (
	VerdictPass  Verdict = "pass"
	VerdictFail  Verdict = "fail"
	VerdictWarn  Verdict = "warn"
	VerdictSkip  Verdict = "skip"
	VerdictError Verdict = "error"
)

func (v Verdict) Valid() bool {
	switch v {
	case VerdictPass, VerdictFail, VerdictWarn, VerdictSkip, VerdictError:
		return true
	}
	return false
}

// Skip reasons are an enumerated set; free text is forbidden (gate B1/B4).
const (
	SkipNoRepo             = "no-repo"             // scanner needs a repository, tool has none
	SkipNoEndpoint         = "no-endpoint"         // conformance needs a remote endpoint
	SkipNoPackage          = "no-package"          // osv needs a published package
	SkipScannerUnavailable = "scanner-unavailable" // binary not installed / API not configured
	SkipNotApplicable      = "not-applicable"      // scanner does not apply to this tool
	SkipTimeout            = "timeout"
	SkipNoData             = "no-data"       // upstream has no result for this tool (e.g. repo not in the Scorecard dataset)
	SkipAuthRequired       = "auth-required" // endpoint wants credentials we do not have
	SkipNeedsConfig        = "needs-config"  // endpoint URL is a template or needs declared headers
)

// MaxRawBytes caps one scanner's raw output (gate B0: 256 KiB).
const MaxRawBytes = 256 << 10

// Result is one benchmark row (schema v1).
type Result struct {
	Scanner        string          `json:"scanner"`
	ScannerVersion string          `json:"scanner_version"`
	ImageDigest    string          `json:"image_digest"`
	RanAt          time.Time       `json:"ran_at"`
	DurationMs     int64           `json:"duration_ms"`
	ExitCode       int             `json:"exit_code"`
	Verdict        Verdict         `json:"verdict"`
	SkipReason     string          `json:"skip_reason,omitempty"`
	Raw            json.RawMessage `json:"raw,omitempty"`
}

// Valid enforces the schema contract (gate B0): provenance present, verdict in
// the enum, skip rows carry a reason and no exit code, raw output bounded.
func (r Result) Valid() error {
	if r.Scanner == "" {
		return errors.New("bench: scanner name is required")
	}
	if r.ScannerVersion == "" {
		return fmt.Errorf("bench: %s: scanner_version is required", r.Scanner)
	}
	if r.RanAt.IsZero() {
		return fmt.Errorf("bench: %s: ran_at is required", r.Scanner)
	}
	if !r.Verdict.Valid() {
		return fmt.Errorf("bench: %s: invalid verdict %q", r.Scanner, r.Verdict)
	}
	if r.Verdict == VerdictSkip && r.SkipReason == "" {
		return fmt.Errorf("bench: %s: skip verdict without skip_reason", r.Scanner)
	}
	if r.Verdict != VerdictSkip && r.SkipReason != "" {
		return fmt.Errorf("bench: %s: skip_reason set on non-skip verdict", r.Scanner)
	}
	if len(r.Raw) > MaxRawBytes {
		return fmt.Errorf("bench: %s: raw output %d bytes exceeds %d", r.Scanner, len(r.Raw), MaxRawBytes)
	}
	return nil
}

// Target is everything a scanner may need about one tool.
type Target struct {
	Slug     string
	RepoURL  string
	Packages []model.Package
	Remotes  []model.Remote
}

// Scanner measures one thing about a tool.
type Scanner interface {
	Name() string
	// Version identifies the scanner (binary version, API date, or builtin tag).
	Version() string
	Run(ctx context.Context, t Target) Result
}

// Runner executes the enabled scanners for one tool.
type Runner struct {
	HTTP         *httpx.Client
	ScorecardAPI string
	OSVAPI       string
	UserAgent    string
	Timeout      time.Duration // per scanner; default 90s
	// AllowPrivate lets conformance reach loopback endpoints (tests only).
	// Production workers always refuse private/loopback/metadata targets.
	AllowPrivate bool
	Log          *slog.Logger
}

func (r *Runner) timeout() time.Duration {
	if r.Timeout <= 0 {
		return 90 * time.Second
	}
	return r.Timeout
}

// Scanners builds the enabled scanner list. Unknown names are ignored so an
// env typo fails loud in logs, not silently in data (B1).
func (r *Runner) Scanners(names []string) []Scanner {
	var out []Scanner
	for _, n := range names {
		switch strings.TrimSpace(n) {
		case "scorecard":
			out = append(out, &Scorecard{API: r.ScorecardAPI, HTTP: r.HTTP, UA: r.UserAgent})
		case "osv":
			out = append(out, &OSV{API: r.OSVAPI, HTTP: r.HTTP, UA: r.UserAgent})
		case "mcp-handshake":
			out = append(out, &Handshake{HTTP: r.HTTP, Log: r.Log, AllowPrivate: r.AllowPrivate})
		case "gitleaks", "trivy", "semgrep", "mcplint":
			out = append(out, &Binary{Bin: n})
		default:
			if r.Log != nil {
				r.Log.Warn("unknown scanner ignored", "scanner", n)
			}
		}
	}
	return out
}

// RunTool runs every scanner and returns one Result per scanner. A scanner
// crash becomes an error verdict, never a silent gap (B1).
func (r *Runner) RunTool(ctx context.Context, scanners []Scanner, t Target) []Result {
	out := make([]Result, 0, len(scanners))
	for _, sc := range scanners {
		sctx, cancel := context.WithTimeout(ctx, r.timeout())
		res := runOne(sctx, sc, t)
		cancel()
		out = append(out, res)
		if r.Log != nil {
			r.Log.Info("benchmark done", "tool", t.Slug, "scanner", res.Scanner, "verdict", string(res.Verdict),
				"skip_reason", res.SkipReason, "ms", res.DurationMs)
		}
	}
	return out
}

func runOne(ctx context.Context, sc Scanner, t Target) (res Result) {
	start := time.Now()
	defer func() {
		if p := recover(); p != nil {
			res = Result{Scanner: sc.Name(), ScannerVersion: sc.Version(), RanAt: time.Now().UTC(),
				DurationMs: time.Since(start).Milliseconds(), ExitCode: 1, Verdict: VerdictError,
				Raw: rawJSON(map[string]any{"panic": fmt.Sprint(p)})}
		}
	}()
	res = sc.Run(ctx, t)
	if res.Scanner == "" {
		res.Scanner = sc.Name()
	}
	if res.ScannerVersion == "" {
		res.ScannerVersion = sc.Version()
	}
	if res.RanAt.IsZero() {
		res.RanAt = time.Now().UTC()
	}
	res.DurationMs = time.Since(start).Milliseconds()
	if ctx.Err() != nil && res.Verdict != VerdictSkip {
		res = Result{Scanner: sc.Name(), ScannerVersion: sc.Version(), RanAt: res.RanAt,
			DurationMs: res.DurationMs, ExitCode: 1, Verdict: VerdictSkip, SkipReason: SkipTimeout}
	}
	return res
}

// rawJSON encodes v for the raw column. Oversized output is replaced by a
// marker instead of being cut, because cut JSON is invalid and the jsonb insert
// would fail.
func rawJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"error": "unencodable output: " + err.Error()})
	}
	if len(b) > MaxRawBytes {
		b, _ = json.Marshal(map[string]any{"truncated": true, "bytes": len(b)})
	}
	return b
}

// Skip returns a skip result with the given reason.
func Skip(sc Scanner, reason string) Result {
	return Result{Scanner: sc.Name(), ScannerVersion: sc.Version(), RanAt: time.Now().UTC(),
		ExitCode: 0, Verdict: VerdictSkip, SkipReason: reason}
}

// Scorecard reads the OpenSSF scorecard API for a GitHub repo.
type Scorecard struct {
	API  string
	HTTP *httpx.Client
	UA   string
}

func (s *Scorecard) Name() string    { return "scorecard" }
func (s *Scorecard) Version() string { return "api-v1" }

func (s *Scorecard) Run(ctx context.Context, t Target) Result {
	base := Result{Scanner: s.Name(), ScannerVersion: s.Version(), RanAt: time.Now().UTC()}
	repo := t.RepoURL
	if repo == "" {
		return Skip(s, SkipNoRepo)
	}
	slug := repoSlug(repo)
	if slug == "" {
		return Skip(s, SkipNoRepo)
	}
	var res struct {
		Score float64 `json:"score"`
		Date  string  `json:"date"`
		Error string  `json:"error"`
	}
	start := time.Now()
	err := s.HTTP.GetJSON(ctx, s.API+"/projects/github.com/"+slug, nil, &res)
	base.DurationMs = time.Since(start).Milliseconds()
	if httpx.IsNotFound(err) {
		// Scorecard only covers repos in its weekly crawl; most MCP repos are not
		// in it. "Not measured" must not read as "failed".
		return Skip(s, SkipNoData)
	}
	if err != nil {
		base.ExitCode, base.Verdict, base.Raw = 1, VerdictError, rawJSON(map[string]any{"error": truncate(err.Error(), 500)})
		return base
	}
	if res.Score == 0 && res.Error != "" {
		base.ExitCode, base.Verdict, base.Raw = 1, VerdictError, rawJSON(map[string]any{"error": truncate(res.Error, 500)})
		return base
	}
	verdict := VerdictPass
	switch {
	case res.Score < 3:
		verdict = VerdictFail
	case res.Score < 5:
		verdict = VerdictWarn
	}
	base.Verdict, base.Raw = verdict, rawJSON(map[string]any{"score": res.Score, "date": res.Date})
	return base
}

// OSV queries osv.dev for each published package (one row per tool, aggregated).
type OSV struct {
	API  string
	HTTP *httpx.Client
	UA   string
}

func (o *OSV) Name() string    { return "osv" }
func (o *OSV) Version() string { return "api-v1" }

func (o *OSV) Run(ctx context.Context, t Target) Result {
	base := Result{Scanner: o.Name(), ScannerVersion: o.Version(), RanAt: time.Now().UTC()}
	if len(t.Packages) == 0 {
		return Skip(o, SkipNoPackage)
	}
	var malicious, vulns []string
	checked := 0
	start := time.Now()
	for _, p := range t.Packages {
		if ctx.Err() != nil {
			break
		}
		eco := ecosystem(p.RegistryType)
		if eco == "" {
			continue
		}
		body := map[string]any{"package": map[string]string{"name": p.Identifier, "ecosystem": eco}}
		if p.Version != "" {
			body["version"] = p.Version
		}
		var res struct {
			Vulns []struct {
				ID string `json:"id"`
			} `json:"vulns"`
		}
		if err := o.HTTP.PostJSON(ctx, o.API+"/v1/query", nil, body, &res); err != nil {
			base.DurationMs = time.Since(start).Milliseconds()
			base.ExitCode, base.Verdict, base.Raw = 1, VerdictError, rawJSON(map[string]any{"error": truncate(err.Error(), 500)})
			return base
		}
		checked++
		for _, v := range res.Vulns {
			if strings.HasPrefix(v.ID, "MAL-") {
				malicious = append(malicious, v.ID)
			} else {
				vulns = append(vulns, v.ID)
			}
		}
	}
	base.DurationMs = time.Since(start).Milliseconds()
	if checked == 0 {
		// Only OCI/other ecosystems OSV does not cover: nothing was measured.
		return Skip(o, SkipNotApplicable)
	}
	verdict := VerdictPass
	switch {
	case len(malicious) > 0:
		verdict = VerdictFail
	case len(vulns) > 0:
		verdict = VerdictWarn
	}
	base.Raw = rawJSON(map[string]any{"malicious": malicious, "vulnerabilities": vulns, "packages_checked": checked})
	base.Verdict = verdict
	return base
}

func ecosystem(registry string) string {
	switch registry {
	case "npm":
		return "npm"
	case "pypi":
		return "PyPI"
	case "crate":
		return "crates.io"
	}
	return ""
}

// Handshake runs mcpkg's own MCP handshake (initialize, tools/list, ...)
// against the tool's remote endpoint, with the same client and SSRF guard as
// the smoke stage. It is NOT the official MCP conformance suite
// (@modelcontextprotocol/conformance), so it is not named after it on the
// public page. stdio-only tools are skipped with a reason code, never "pass".
type Handshake struct {
	HTTP         *httpx.Client
	Log          *slog.Logger
	AllowPrivate bool // tests only
}

func (c *Handshake) Name() string    { return "mcp-handshake" }
func (c *Handshake) Version() string { return "mcpkg/" + mcpclient.LatestProtocol }

func (c *Handshake) Run(ctx context.Context, t Target) Result {
	base := Result{Scanner: c.Name(), ScannerVersion: c.Version(), RanAt: time.Now().UTC()}
	if len(t.Remotes) == 0 {
		return Skip(c, SkipNoEndpoint)
	}
	remote := t.Remotes[0]
	if strings.Contains(remote.URL, "{") || len(remote.Headers) > 0 {
		return Skip(c, SkipNeedsConfig) // templated URL or declared headers: we cannot fill them in
	}
	client := mcpclient.SafeHTTPClient(40*time.Second, c.AllowPrivate)
	start := time.Now()
	info, err := mcpclient.HandshakeHTTPLog(ctx, client, remote.URL, remote.Type, nil, c.Log)
	base.DurationMs = time.Since(start).Milliseconds()
	if errors.Is(err, mcpclient.ErrAuthRequired) {
		return Skip(c, SkipAuthRequired)
	}
	if err != nil {
		base.ExitCode, base.Verdict, base.Raw = 1, VerdictFail, rawJSON(map[string]any{"error": truncate(err.Error(), 500)})
		return base
	}
	base.Raw = rawJSON(map[string]any{
		"protocol": info.ProtocolVersion, "server": info.ServerName, "server_version": info.ServerVersion,
		"tools": len(info.Tools), "prompts": info.PromptsCount, "resources": info.ResourcesCount,
		"stdout_pollution": info.StdoutPollution, "init_ms": info.InitMs,
	})
	base.Verdict = VerdictPass
	if info.ToolsError != "" {
		base.Verdict = VerdictWarn
	}
	return base
}

// Binary adapts a CLI scanner (gitleaks, trivy, semgrep, mcplint). Without the
// binary installed it reports skip/scanner-unavailable, never pass (B1).
type Binary struct {
	Bin string
}

// Name implements Scanner.
func (b *Binary) Name() string { return b.Bin }

func (b *Binary) Version() string { return "cli" }

func (b *Binary) Run(ctx context.Context, t Target) Result {
	// Binary scanners need a checkout, which heavy workers provide via
	// BENCH_REPO_DIR; without one the scanner honestly reports unavailable.
	return Result{Scanner: b.Bin, ScannerVersion: b.Version(), RanAt: time.Now().UTC(),
		Verdict: VerdictSkip, SkipReason: SkipScannerUnavailable}
}

// repoSlug turns a GitHub https URL into owner/name ("" when not a GitHub repo).
func repoSlug(u string) string {
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	parts := strings.Split(u, "/")
	if len(parts) < 3 || parts[0] != "github.com" {
		return ""
	}
	return parts[1] + "/" + parts[2]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
