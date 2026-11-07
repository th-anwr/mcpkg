// Package app wires configuration, database, queue, stages and sources together.
// It is shared by the CLI and the integration tests.
package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mcpkg/mcpkg/internal/config"
	"github.com/mcpkg/mcpkg/internal/db"
	"github.com/mcpkg/mcpkg/internal/discover"
	"github.com/mcpkg/mcpkg/internal/httpx"
	"github.com/mcpkg/mcpkg/internal/llm"
	"github.com/mcpkg/mcpkg/internal/pipeline"
	"github.com/mcpkg/mcpkg/internal/queue"
	"github.com/mcpkg/mcpkg/internal/sandbox"
	"github.com/mcpkg/mcpkg/internal/sources"
	"github.com/mcpkg/mcpkg/internal/stages"
	"github.com/mcpkg/mcpkg/internal/store"
	"github.com/mcpkg/mcpkg/internal/submit"
)

// App holds shared dependencies.
type App struct {
	Cfg   config.Config
	Log   *slog.Logger
	DB    *sql.DB
	Store *store.Store
	Queue *queue.Queue
	HTTP  *httpx.Client
	Pipe  pipeline.Config
	// AllowPrivateRemotes lets smoke tests reach localhost endpoints (tests only).
	AllowPrivateRemotes bool
}

// Open connects to the database and loads pipeline.json.
func Open(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	conn, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	pipe, err := pipeline.Load(cfg.PipelineFile)
	if err != nil {
		return nil, err
	}
	return New(cfg, log, conn, pipe), nil
}

// New builds an App from an existing connection.
func New(cfg config.Config, log *slog.Logger, conn *sql.DB, pipe pipeline.Config) *App {
	q := queue.New(conn)
	q.Log = log.With("component", "queue")
	h := httpx.New(cfg.UserAgent)
	h.Log = log.With("component", "http")
	st := store.New(conn)
	st.Log = log.With("component", "store")
	return &App{Cfg: cfg, Log: log, DB: conn, Store: st, Queue: q, HTTP: h, Pipe: pipe}
}

// Discoverer returns the candidate merger.
func (a *App) Discoverer() *discover.Discoverer {
	return &discover.Discoverer{Store: a.Store, Queue: a.Queue, EntryStage: a.Pipe.Entry, Log: a.Log}
}

// Resolver handles author submissions.
func (a *App) Resolver() *submit.Resolver {
	return &submit.Resolver{Cfg: a.Cfg, HTTP: a.HTTP, Disc: a.Discoverer(), Log: a.Log.With("component", "submit")}
}

// SandboxRunner builds the configured smoke-test sandbox (nil when disabled).
func (a *App) SandboxRunner() sandbox.Runner {
	switch a.Cfg.Sandbox {
	case "docker":
		return &sandbox.DockerRunner{Image: a.Cfg.SandboxImage, Runtime: a.Cfg.SandboxRuntime, EgressNetwork: a.Cfg.SandboxEgressNetwork,
			Proxy: a.Cfg.SandboxProxy, Memory: a.Cfg.SandboxMemory, CPUs: a.Cfg.SandboxCPUs, Log: a.Log.With("component", "sandbox")}
	case "process":
		a.Log.Warn("SANDBOX=process runs untrusted servers directly on this machine: use only for local development")
		return &sandbox.ProcessRunner{BaseDir: a.Cfg.WorkDir, Log: a.Log.With("component", "sandbox")}
	}
	return nil
}

// Runner builds the pipeline with every stage wired.
func (a *App) Runner() *pipeline.Runner {
	deps := &stages.Deps{Cfg: a.Cfg, HTTP: a.HTTP, Store: a.Store, Gates: a.Pipe.Gates, Pipe: a.Pipe, Log: a.Log}
	model := &llm.Client{BaseURL: a.Cfg.LLMBaseURL, APIKey: a.Cfg.LLMAPIKey, Model: a.Cfg.LLMModel, JSONMode: a.Cfg.LLMJSONMode, HTTP: a.HTTP,
		Log: a.Log.With("component", "llm")}
	st := map[string]pipeline.Stage{
		"health":   &stages.Health{D: deps},
		"security": &stages.Security{D: deps},
		"smoke":    &stages.Smoke{D: deps, Runner: a.SandboxRunner(), AllowPrivate: a.AllowPrivateRemotes},
		"enrich":   &stages.Enrich{D: deps, LLM: model},
		"render":   &stages.Render{D: deps, Queue: a.Queue},
	}
	return &pipeline.Runner{Store: a.Store, Queue: a.Queue, Config: a.Pipe, Stages: st,
		Hooks: pipeline.NewHooks(a.Pipe.Hooks), WorkerID: a.Cfg.WorkerID, Log: a.Log}
}

// Sources builds crawlers by name.
func (a *App) Sources(names []string) ([]sources.Source, error) {
	var out []sources.Source
	for _, n := range names {
		switch strings.TrimSpace(n) {
		case "registry":
			out = append(out, &sources.Registry{BaseURL: a.Cfg.RegistryURL, HTTP: a.HTTP, Log: a.Log.With("component", "source")})
		case "npm":
			out = append(out, &sources.Npm{Registry: a.Cfg.NpmRegistry, HTTP: a.HTTP, Log: a.Log.With("component", "source")})
		case "pypi":
			out = append(out, &sources.PyPI{BaseURL: a.Cfg.PyPIURL, HTTP: a.HTTP, Log: a.Log.With("component", "source")})
		case "crates":
			out = append(out, &sources.Crates{API: a.Cfg.CratesAPI, HTTP: a.HTTP, Log: a.Log.With("component", "source")})
		case "github":
			out = append(out, &sources.GitHub{API: a.Cfg.GitHubAPI, Token: a.Cfg.GitHubToken, HTTP: a.HTTP, Log: a.Log.With("component", "source")})
		case "":
		default:
			return nil, fmt.Errorf("unknown source %q", n)
		}
	}
	return out, nil
}
