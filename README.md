# mcpkg

**npm-style catalog for open-source MCP servers.** Every server listed is checked first: repo health, a security scan of the published package, and a sandboxed smoke test. Each one then gets a page with exact install steps for every MCP client, readable by both people and agents.

> Working name: rename with `SITE_NAME` / `META_NAMESPACE` in `.env` and the Go module path.

```
discover ──► health ──► security ──► smoke ──► enrich ──► render ──► page + API
(registry,    (GitHub     (npm tarball / (sandbox,    (open LLM,   (score,
 npm, PyPI,    GraphQL,    wheel; OSV,    no network,  validated)   install
 crates,       no code     Scorecard)     MCP hand-                 manifest)
 GitHub)       runs)                      shake)
```

## What you get

| Piece | Where |
|---|---|
| Crawlers: official MCP Registry (hourly, incremental), npm, PyPI, crates.io, GitHub topics | `internal/sources` |
| Dedupe across sources (same server found on npm + registry + GitHub = one entry) | `internal/discover` |
| Postgres job queue with leases, retries and priority (no Redis/SQS) | `internal/queue` |
| Pipeline **hooks**: health → security → smoke → enrich → render, with gates | `internal/stages`, `pipeline.json` |
| Your own hooks in any language (stdin/stdout JSON) | `hooks/examples`, [docs/HOOKS.md](docs/HOOKS.md) |
| Sandboxed smoke test (Docker + optional gVisor, no network at runtime) | `internal/sandbox`, `sandbox/` |
| MCP client: stdio, Streamable HTTP, legacy SSE, SSRF-safe | `internal/mcpclient` |
| Deterministic trust score A–F | `internal/score`, [docs/SCORING.md](docs/SCORING.md) |
| Install generator: Claude Code, Claude Desktop, Cursor, VS Code, Codex | `internal/installgen` |
| npm-style site: **one template per page type, filled from data** | `internal/web/templates` |
| Agent endpoints: `install.md`, `manifest.json`, `/api/search`, `/llms.txt` | `internal/web` |
| MCP Registry-compatible API (`/v0.1/servers` + `_meta` trust data) | `internal/web/api.go` |
| README badges (`/badge/<slug>.svg`) | `internal/web/api.go` |
| Finder MCP server for agents (`search_tools`, `get_install_instructions`) | `internal/finder` |
| AWS deploy kit sized for ~$114 of credits | `deploy/`, [docs/PLAN.md](docs/PLAN.md) |

## Quick start (local)

```bash
# Postgres in Docker, web on :8080, smoke tests in "process" mode (not isolated: dev only)
./scripts/dev.sh
./scripts/dev.sh worker          # second terminal: runs the pipeline
```

Or by hand:

```bash
export DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5433/mcpkg?sslmode=disable
go run ./cmd/mcpkg migrate
go run ./cmd/mcpkg discover -source registry,npm -limit 50
SANDBOX=process go run ./cmd/mcpkg worker
go run ./cmd/mcpkg serve
go run ./cmd/mcpkg run -slug <slug>     # push one tool through every stage and print results
```

## Deploy (AWS, from CloudShell)

Step-by-step runbook with checkpoints and troubleshooting: **[docs/DEPLOY.md](docs/DEPLOY.md)**.

```bash
./scripts/make-env.sh                         # .env with random passwords + your GitHub token
./deploy/aws/launch.sh core                   # t4g.small + Elastic IP (key pair + security group created on first run)
./deploy/aws/push.sh core                     # copy code, install Docker, build, start; discovery starts by itself
./deploy/aws/launch.sh worker                 # c7a.xlarge spot (repeat for more)
./deploy/aws/push.sh worker <worker-ip>       # security + smoke tests from the shared queue
./deploy/aws/ssh.sh core sudo /opt/mcpkg/scripts/status.sh
./deploy/aws/teardown.sh workers              # after the first full pass
```

Plan, timeline and budget: [docs/PLAN.md](docs/PLAN.md). Architecture: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Commands

```
mcpkg migrate | discover | worker | scheduler | serve | enqueue | run | add | stats | finder | eval-llm
```

`mcpkg help` lists flags. Everything is configured with environment variables (`.env.example`).

## How pages work

There are no generated page files. `internal/web/templates/tool.html` is the one placeholder template for every server page. The pipeline fills the data (README, live `tools/list` output, scores, install manifest), and the template renders it on request. Adding 50,000 tools means adding 50,000 rows, not 50,000 pages. `install.md` (for agents) comes from the same data through `templates/install.md.tmpl`.

## Registering a server

Authors can do either:

1. Publish to the official MCP Registry. It's picked up within an hour, including declared env vars and arguments, which makes install steps exact.
2. Paste a link at `/submit` (GitHub repo or monorepo folder, npm/PyPI page, or https endpoint), or run `mcpkg add <url>`.

## Tests

```bash
go test ./...                                                     # unit tests
TEST_DATABASE_URL=postgres://...mcpkg_test go test ./internal/e2e # queue + dedupe on Postgres
TEST_DATABASE_URL=... E2E_NETWORK=1 go test ./internal/e2e -run TestFullPipeline -v
```

The end-to-end test installs real packages from npm and PyPI. It runs every stage against fake registry/GitHub/OSV/LLM endpoints, then checks the rendered pages and APIs.

## Security model (short version)

- Untrusted code only runs in the smoke stage, in throwaway containers with `--network none`, a read-only rootfs, no capabilities, and pid/memory/CPU limits. gVisor is optional. Installs go through an allowlist proxy that reaches npm, PyPI and GitHub only.
- Remote endpoint checks refuse private, loopback and metadata IPs (SSRF guard). EC2 metadata hop limit is 1.
- The LLM never decides scores, decisions or install commands. Its output is validated against the input and falls back to deterministic text.
- Static findings send a server to human review. Only OSV `MAL-*` advisories and registry takedowns hide a server automatically.

Details: [docs/ARCHITECTURE.md#security](docs/ARCHITECTURE.md#security).
