// Package config loads settings from environment variables (see .env.example).
package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// Config is the full runtime configuration. Every field has a safe default
// except secrets, which are optional (features degrade gracefully without them).
type Config struct {
	DatabaseURL string

	SiteName      string // shown in the header and page titles
	SiteURL       string // absolute base URL, used in manifests and badges
	MetaNamespace string // reverse-DNS key for subregistry _meta, e.g. "dev.mcpkg"
	ContactEmail  string // sent in User-Agent (crates.io requires contact info)
	UserAgent     string

	// Upstream APIs (overridable for tests and mirrors).
	RegistryURL  string
	GitHubAPI    string
	GitHubToken  string
	RawGitHub    string
	NpmRegistry  string
	NpmAPI       string
	JsDelivr     string
	PyPIURL      string
	CratesAPI    string
	OSVAPI       string
	ScorecardAPI string

	// LLM (any OpenAI-compatible endpoint: vLLM, Ollama, DeepInfra, Together,
	// Groq, OpenRouter, Bedrock's OpenAI-compatible endpoint, ...).
	LLMBaseURL      string
	LLMAPIKey       string
	LLMModel        string
	LLMJSONMode     bool
	LLMReviewSample float64 // fraction of LLM outputs sent to the human review queue

	// Sandbox for smoke tests.
	Sandbox              string // docker | process | off
	SandboxImage         string
	SandboxRuntime       string // "" (runc) or "runsc" (gVisor)
	SandboxEgressNetwork string // docker network used during install only
	SandboxProxy         string // egress allowlist proxy used during install
	SandboxMemory        string
	SandboxCPUs          string

	// Benchmarks stage (docs/BENCH_GATE.md). Off until the gate is green;
	// the Benchmarks tab stays hidden without rows.
	BenchEnabled    bool   // BENCH_ENABLED
	BenchScanners   string // BENCH_SCANNERS comma list
	BenchTimeoutSec int    // BENCH_TIMEOUT_SEC per scanner
	BenchRepoDir    string // BENCH_REPO_DIR checkout cache for CLI scanners

	PipelineFile  string
	WorkerID      string
	WorkDir       string
	AdminPassword string
	Addr          string
	LogLevel      string
}

// Load reads the environment.
func Load() Config {
	c := Config{
		DatabaseURL:   env("DATABASE_URL", DefaultDatabaseURL),
		SiteName:      env("SITE_NAME", "mcpkg"),
		SiteURL:       strings.TrimRight(env("SITE_URL", "http://localhost:8080"), "/"),
		MetaNamespace: env("META_NAMESPACE", "dev.mcpkg"),
		ContactEmail:  os.Getenv("CONTACT_EMAIL"),

		RegistryURL:  strings.TrimRight(env("REGISTRY_URL", "https://registry.modelcontextprotocol.io"), "/"),
		GitHubAPI:    strings.TrimRight(env("GITHUB_API", "https://api.github.com"), "/"),
		GitHubToken:  os.Getenv("GITHUB_TOKEN"),
		RawGitHub:    strings.TrimRight(env("RAW_GITHUB", "https://raw.githubusercontent.com"), "/"),
		NpmRegistry:  strings.TrimRight(env("NPM_REGISTRY", "https://registry.npmjs.org"), "/"),
		NpmAPI:       strings.TrimRight(env("NPM_API", "https://api.npmjs.org"), "/"),
		JsDelivr:     strings.TrimRight(env("JSDELIVR", "https://cdn.jsdelivr.net"), "/"),
		PyPIURL:      strings.TrimRight(env("PYPI_URL", "https://pypi.org"), "/"),
		CratesAPI:    strings.TrimRight(env("CRATES_API", "https://crates.io"), "/"),
		OSVAPI:       strings.TrimRight(env("OSV_API", "https://api.osv.dev"), "/"),
		ScorecardAPI: strings.TrimRight(env("SCORECARD_API", "https://api.securityscorecards.dev"), "/"),

		LLMBaseURL:      strings.TrimRight(os.Getenv("LLM_BASE_URL"), "/"),
		LLMAPIKey:       os.Getenv("LLM_API_KEY"),
		LLMModel:        env("LLM_MODEL", "openai/gpt-oss-20b"),
		LLMJSONMode:     envBool("LLM_JSON_MODE", true),
		LLMReviewSample: envFloat("LLM_REVIEW_SAMPLE", 0.01),

		Sandbox:              env("SANDBOX", "docker"),
		SandboxImage:         env("SANDBOX_IMAGE", "mcpkg-runner:latest"),
		SandboxRuntime:       os.Getenv("SANDBOX_RUNTIME"),
		SandboxEgressNetwork: env("SANDBOX_EGRESS_NETWORK", "mcpkg-egress"),
		SandboxProxy:         env("SANDBOX_PROXY", "http://egress-proxy:3128"),
		SandboxMemory:        env("SANDBOX_MEMORY", "1g"),
		SandboxCPUs:          env("SANDBOX_CPUS", "1"),

		BenchEnabled:    envBool("BENCH_ENABLED", false),
		BenchScanners:   env("BENCH_SCANNERS", "scorecard,osv,mcp-handshake"),
		BenchTimeoutSec: envInt("BENCH_TIMEOUT_SEC", 90),
		BenchRepoDir:    os.Getenv("BENCH_REPO_DIR"),

		PipelineFile:  env("PIPELINE_FILE", "pipeline.json"),
		WorkerID:      env("WORKER_ID", hostname()),
		WorkDir:       env("WORK_DIR", os.TempDir()),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
		Addr:          env("ADDR", ":8080"),
		LogLevel:      env("LOG_LEVEL", "info"),
	}
	contact := "+" + c.SiteURL
	if c.ContactEmail != "" {
		contact += "; " + c.ContactEmail
	}
	c.UserAgent = env("USER_AGENT", c.SiteName+"-indexer/0.1 ("+contact+")")
	return c
}

// DefaultDatabaseURL is the local-development database. Production must set its own.
const DefaultDatabaseURL = "postgres://postgres:postgres@127.0.0.1:5432/mcpkg?sslmode=disable"

// Validate rejects configurations that only make sense in development but
// look like production (an https SITE_URL).
func (c Config) Validate() error {
	if strings.HasPrefix(c.SiteURL, "https://") && c.DatabaseURL == DefaultDatabaseURL {
		return errors.New("SITE_URL is https but DATABASE_URL is the development default: set DATABASE_URL in .env")
	}
	return nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envFloat(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "worker"
	}
	return h
}
