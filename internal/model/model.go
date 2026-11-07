// Package model holds the shared types used across discovery, the pipeline,
// scoring, install generation and the website. Package/Remote mirror the
// official MCP Registry server.json schema so registry data round-trips.
package model

import (
	"encoding/json"
	"strings"
	"time"
)

// Package is one installable distribution of an MCP server (server.json "packages[]").
type Package struct {
	RegistryType         string     `json:"registryType"` // npm | pypi | oci | nuget | mcpb | crate
	Identifier           string     `json:"identifier"`
	Version              string     `json:"version,omitempty"`
	RegistryBaseURL      string     `json:"registryBaseUrl,omitempty"`
	RuntimeHint          string     `json:"runtimeHint,omitempty"`
	Transport            Transport  `json:"transport"`
	EnvironmentVariables []EnvVar   `json:"environmentVariables,omitempty"`
	PackageArguments     []Argument `json:"packageArguments,omitempty"`
	RuntimeArguments     []Argument `json:"runtimeArguments,omitempty"`
	// Bin is the executable resolved by the smoke test (npm bin / Python entry point).
	Bin string `json:"bin,omitempty"`
}

// Transport describes how a client talks to a package once it runs.
type Transport struct {
	Type    string     `json:"type"` // stdio | streamable-http | sse
	URL     string     `json:"url,omitempty"`
	Headers []KeyValue `json:"headers,omitempty"`
}

// Remote is a hosted endpoint (server.json "remotes[]").
type Remote struct {
	Type    string     `json:"type"` // streamable-http | sse
	URL     string     `json:"url"`
	Headers []KeyValue `json:"headers,omitempty"`
}

// EnvVar is an environment variable a server reads (often an API key).
type EnvVar struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	IsRequired  bool     `json:"isRequired,omitempty"`
	IsSecret    bool     `json:"isSecret,omitempty"`
	Default     string   `json:"default,omitempty"`
	Format      string   `json:"format,omitempty"`
	Choices     []string `json:"choices,omitempty"`
}

// KeyValue is a header or similar named input.
type KeyValue struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Value       string `json:"value,omitempty"`
	IsRequired  bool   `json:"isRequired,omitempty"`
	IsSecret    bool   `json:"isSecret,omitempty"`
}

// Argument is a CLI argument passed to the package or its runtime.
type Argument struct {
	Type        string `json:"type"` // positional | named
	Name        string `json:"name,omitempty"`
	ValueHint   string `json:"valueHint,omitempty"`
	Value       string `json:"value,omitempty"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
	Format      string `json:"format,omitempty"`
	IsRequired  bool   `json:"isRequired,omitempty"`
	IsRepeated  bool   `json:"isRepeated,omitempty"`
}

// Tool is one catalog entry (one MCP server), as stored in the tools table.
type Tool struct {
	ID              int64                      `json:"id"`
	Slug            string                     `json:"slug"`
	Name            string                     `json:"name"`
	Title           string                     `json:"title"`
	Description     string                     `json:"description"`
	Version         string                     `json:"version"`
	RepoURL         string                     `json:"repoUrl"`
	RepoSubfolder   string                     `json:"repoSubfolder"`
	WebsiteURL      string                     `json:"websiteUrl"`
	Packages        []Package                  `json:"packages"`
	Remotes         []Remote                   `json:"remotes"`
	Sources         map[string]json.RawMessage `json:"sources"`
	Readme          string                     `json:"-"`
	ReadmeURL       string                     `json:"readmeUrl"`
	Status          string                     `json:"status"`     // new | active | unmaintained | blocked | missing | deprecated
	Visibility      string                     `json:"visibility"` // listed | hidden
	SmokeStatus     string                     `json:"smokeStatus"`
	Health          *Health                    `json:"health,omitempty"`
	Security        *Security                  `json:"security,omitempty"`
	Smoke           *Smoke                     `json:"smoke,omitempty"`
	Enrich          *Enrich                    `json:"enrich,omitempty"`
	Score           *Score                     `json:"score,omitempty"`
	Install         *Manifest                  `json:"install,omitempty"`
	ScoreTotal      *int                       `json:"scoreTotal,omitempty"`
	Grade           string                     `json:"grade"`
	Stars           int                        `json:"stars"`
	WeeklyDownloads int64                      `json:"weeklyDownloads"`
	LastCommitAt    *time.Time                 `json:"lastCommitAt,omitempty"`
	PublishedAt     *time.Time                 `json:"publishedAt,omitempty"`
	VerifiedAt      *time.Time                 `json:"verifiedAt,omitempty"`
	Categories      []string                   `json:"categories"`
	Summary         string                     `json:"summary"`
	FirstSeenAt     time.Time                  `json:"firstSeenAt"`
	UpdatedAt       time.Time                  `json:"updatedAt"`
}

// DisplayName is what pages show as the headline.
func (t *Tool) DisplayName() string {
	if t.Title != "" {
		return t.Title
	}
	// "io.github.org/time" -> "time"; the full name is shown next to it.
	if i := strings.LastIndex(t.Name, "/"); i >= 0 && i < len(t.Name)-1 {
		return t.Name[i+1:]
	}
	return t.Name
}

// RegistryStatus returns the official registry status (active/deprecated/deleted) if known.
func (t *Tool) RegistryStatus() string {
	raw, ok := t.Sources["registry"]
	if !ok {
		return ""
	}
	var m struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Status
}

// Health is the result of the repo-health stage (no code is executed).
type Health struct {
	Provider         string     `json:"provider"` // github | none
	Repo             string     `json:"repo,omitempty"`
	RepoFound        bool       `json:"repoFound"`
	Archived         bool       `json:"archived"`
	Disabled         bool       `json:"disabled"`
	Fork             bool       `json:"fork"`
	Stars            int        `json:"stars"`
	Forks            int        `json:"forks"`
	Contributors     int        `json:"contributors"`
	OpenIssues       int        `json:"openIssues"`
	ClosedIssues     int        `json:"closedIssues"`
	OpenPRs          int        `json:"openPRs"`
	MergedPRs        int        `json:"mergedPRs"`
	ClosedPRs        int        `json:"closedPRs"`
	OldestOpenPRAt   *time.Time `json:"oldestOpenPRAt,omitempty"`
	LastCommitAt     *time.Time `json:"lastCommitAt,omitempty"`
	LastReleaseAt    *time.Time `json:"lastReleaseAt,omitempty"`
	LastReleaseTag   string     `json:"lastReleaseTag,omitempty"`
	CreatedAt        *time.Time `json:"createdAt,omitempty"`
	RecentCommits90d int        `json:"recentCommits90d"`
	License          string     `json:"license,omitempty"`
	OwnerType        string     `json:"ownerType,omitempty"`
	OwnerVerified    bool       `json:"ownerVerified"`
	Topics           []string   `json:"topics,omitempty"`
	WeeklyDownloads  int64      `json:"weeklyDownloads"`
	HeadCommit       string     `json:"headCommit,omitempty"`
	Partial          bool       `json:"partial"` // true when data came from a fallback path
	Notes            []string   `json:"notes,omitempty"`
	CheckedAt        time.Time  `json:"checkedAt"`
}

// Finding is one security or quality observation.
type Finding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"` // critical | high | medium | low | info
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Snippet  string `json:"snippet,omitempty"`
	Message  string `json:"message"`
}

// VulnCounts summarizes dependency vulnerabilities.
type VulnCounts struct {
	Critical  int      `json:"critical"`
	High      int      `json:"high"`
	Medium    int      `json:"medium"`
	Low       int      `json:"low"`
	Malicious []string `json:"malicious,omitempty"`
}

// Security is the result of the static security stage (no code is executed).
type Security struct {
	Scanned        string         `json:"scanned"` // npm-tarball | pypi-dist | git | none
	Target         string         `json:"target,omitempty"`
	Files          int            `json:"files"`
	Findings       []Finding      `json:"findings,omitempty"`
	Capabilities   []string       `json:"capabilities,omitempty"` // shell, network, fs_write, env ...
	InstallScripts []string       `json:"installScripts,omitempty"`
	EntryPoints    []string       `json:"entryPoints,omitempty"` // Python console_scripts
	Vulns          VulnCounts     `json:"vulns"`
	Scorecard      *float64       `json:"scorecard,omitempty"`
	Scanners       []string       `json:"scanners,omitempty"`
	Counts         map[string]int `json:"counts,omitempty"`
	Blocked        bool           `json:"blocked"`
	BlockReason    string         `json:"blockReason,omitempty"`
	NeedsReview    bool           `json:"needsReview"`
	Notes          []string       `json:"notes,omitempty"`
	CheckedAt      time.Time      `json:"checkedAt"`
}

// ToolInfo is one tool reported by the server's tools/list.
type ToolInfo struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
}

// Smoke is the result of the sandboxed smoke test.
type Smoke struct {
	Status              string     `json:"status"`           // works | needs_credentials | auth_required | broken | install_failed | skipped
	Method              string     `json:"method,omitempty"` // npm | pypi | oci | remote
	Command             string     `json:"command,omitempty"`
	Package             string     `json:"package,omitempty"`
	Version             string     `json:"version,omitempty"`
	Bin                 string     `json:"bin,omitempty"`
	InstallMs           int64      `json:"installMs,omitempty"`
	StartupMs           int64      `json:"startupMs,omitempty"`
	ProtocolVersion     string     `json:"protocolVersion,omitempty"`
	ServerName          string     `json:"serverName,omitempty"`
	ServerVersion       string     `json:"serverVersion,omitempty"`
	Capabilities        []string   `json:"capabilities,omitempty"`
	Tools               []ToolInfo `json:"tools,omitempty"`
	PromptsCount        int        `json:"promptsCount"`
	ResourcesCount      int        `json:"resourcesCount"`
	StdoutPollution     bool       `json:"stdoutPollution"`
	CredentialHint      bool       `json:"credentialHint"`
	DescriptionFindings []Finding  `json:"descriptionFindings,omitempty"`
	SkipReason          string     `json:"skipReason,omitempty"`
	Error               string     `json:"error,omitempty"`
	LogTail             string     `json:"logTail,omitempty"`
	Sandbox             string     `json:"sandbox,omitempty"`
	CheckedAt           time.Time  `json:"checkedAt"`
}

// Enrich holds LLM-written text. It never influences scores or install steps.
type Enrich struct {
	Summary      string            `json:"summary"`
	Description  string            `json:"description"`
	Categories   []string          `json:"categories"`
	EnvHelp      map[string]string `json:"envHelp,omitempty"`
	Caveats      []string          `json:"caveats,omitempty"`
	Source       string            `json:"source"` // llm | fallback
	Model        string            `json:"model,omitempty"`
	InputTokens  int               `json:"inputTokens,omitempty"`
	OutputTokens int               `json:"outputTokens,omitempty"`
	Rejected     []string          `json:"rejected,omitempty"` // validation failures that triggered fallback
	CheckedAt    time.Time         `json:"checkedAt"`
}

// Score is the deterministic trust score.
type Score struct {
	Total       int            `json:"total"`
	Grade       string         `json:"grade"`
	Maintenance int            `json:"maintenance"`
	Security    int            `json:"security"`
	Works       int            `json:"works"`
	Community   int            `json:"community"`
	Caps        []string       `json:"caps,omitempty"`
	Reasons     []string       `json:"reasons,omitempty"`
	Weights     map[string]int `json:"weights"`
	Version     string         `json:"version"`
	ComputedAt  time.Time      `json:"computedAt"`
}

// Manifest is the machine-readable install recipe served to agents.
type Manifest struct {
	Schema     string          `json:"$schema"`
	Name       string          `json:"name"`
	Slug       string          `json:"slug"`
	Title      string          `json:"title"`
	Summary    string          `json:"summary"`
	Version    string          `json:"version,omitempty"`
	ServerKey  string          `json:"serverKey"`
	PageURL    string          `json:"pageUrl"`
	InstallMD  string          `json:"installMdUrl"`
	Trust      ManifestTrust   `json:"trust"`
	Methods    []InstallMethod `json:"methods"`
	Env        []ManifestEnv   `json:"env"`
	Expected   []string        `json:"expectedTools,omitempty"`
	AgentSteps []string        `json:"agentSteps"`
	Warnings   []string        `json:"warnings,omitempty"`
	Generated  time.Time       `json:"generatedAt"`
}

// ManifestTrust is the trust summary embedded in a manifest.
type ManifestTrust struct {
	Score       int        `json:"score"`
	Grade       string     `json:"grade"`
	SmokeStatus string     `json:"smokeStatus"`
	Status      string     `json:"status"`
	VerifiedAt  *time.Time `json:"verifiedAt,omitempty"`
}

// ManifestEnv is a value the user must provide.
type ManifestEnv struct {
	Name        string `json:"name"`
	Required    bool   `json:"required"`
	Secret      bool   `json:"secret"`
	Description string `json:"description,omitempty"`
	Help        string `json:"help,omitempty"`
	Default     string `json:"default,omitempty"`
	Placeholder string `json:"placeholder"`
}

// InstallMethod is one way to run the server, with per-client configs.
type InstallMethod struct {
	ID        string            `json:"id"` // npm | pypi | oci | remote
	Label     string            `json:"label"`
	Transport string            `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   []KeyValue        `json:"headers,omitempty"`
	Clients   []ClientConfig    `json:"clients"`
	Requires  []string          `json:"requires,omitempty"` // e.g. "Node.js 18+", "uv", "Docker"
	Extra     map[string]string `json:"extra,omitempty"`
}

// ClientConfig is a ready-to-paste config or command for one MCP client.
type ClientConfig struct {
	Client   string `json:"client"` // claude-code | claude-desktop | cursor | vscode | codex
	Label    string `json:"label"`
	Kind     string `json:"kind"` // cli | json | toml
	Location string `json:"location,omitempty"`
	Content  string `json:"content"`
}
