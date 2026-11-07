# mcpkg handoff (read this first; open other files only when a task needs them)

## What it is
npm-style catalog of open-source MCP servers. Every server is discovered, checked (health → security → smoke → enrich → render), scored A–F, and gets a page with exact install steps for people and agents. Working name "mcpkg" (domain not chosen; mcp.net likely taken).

## Status (2026-11-07)
- Code complete, all tests pass: unit, Postgres, end-to-end with real npm/PyPI installs.
- **Not deployed yet.** Plan: AWS CloudShell tonight, following `docs/DEPLOY.md`.
- The AWS scripts were only dry-run against a fake AWS CLI. Docker images have never been built: the build sandbox blocked image registries.

## Stack and decisions
- One Go binary (`cmd/mcpkg`), Postgres only (it also holds the job queue: SKIP LOCKED, leases, priority). Deps: `lib/pq`, `goldmark`.
- Pages are Go templates filled from the DB (`internal/web/templates/tool.html`); no generated files.
- Smoke test runs in Docker (install via egress-allowlist proxy, then `--network none`, optional gVisor). `SANDBOX=process` is for dev only.
- The LLM writes only descriptions. It is validated against the README and falls back to plain text. It never touches scores or install commands. Any OpenAI-compatible endpoint works (Bedrock mantle, DeepInfra, vLLM).
- Scores are deterministic (`internal/score`, formula in `docs/SCORING.md`).
- AWS budget: $114 in credits. Core box: t4g.small on-demand with an Elastic IP. Workers: c7a.xlarge spot ×2, torn down after the first pass.

## File map
| Area | Path |
|---|---|
| CLI and wiring | `cmd/mcpkg/main.go`, `internal/app/app.go` |
| Crawlers | `internal/sources/` (registry, npm, pypi, crates, github) |
| Dedupe and merge | `internal/discover/discover.go` |
| Pipeline and hooks | `internal/pipeline/`, `pipeline.json`, `docs/HOOKS.md` |
| Stages | `internal/stages/` (health, security, smoke, enrich, render) |
| Sandbox / MCP client | `internal/sandbox/`, `internal/mcpclient/`, `internal/launch/` |
| Scan rules | `internal/scan/`, `scanners/semgrep/mcp-rules.yml` |
| Install generator | `internal/installgen/` |
| Website / API | `internal/web/` (templates, static, api.go = badges + `/v0.1/servers`) |
| DB schema | `internal/db/migrations/` |
| Deploy | `deploy/` (Dockerfile, compose, Caddyfile), `deploy/aws/*.sh`, `sandbox/` |
| Ops scripts | `scripts/` (make-env, status, progress, mcpkg, sql, compose, backup) |
| Docs | `docs/PLAN.md`, `ARCHITECTURE.md`, `DEPLOY.md`, `LLM.md`, `SCORING.md`, `INSTALL_FORMAT.md` |

## Commands
```
go test ./...                                                        # unit
TEST_DATABASE_URL=postgres://...mcpkg_test go test ./internal/e2e    # DB
E2E_NETWORK=1 (same) go test ./internal/e2e -run TestFullPipeline    # real installs
```

## Next tasks (agreed or offered)
1. Deploy tonight (`docs/DEPLOY.md`), then fix whatever breaks.
2. **Benchmarks stage plus a "Benchmarks" tab** showing raw results, tool version and run date for each tool. Run on every repo:
   - MCP conformance suite (`npx @modelcontextprotocol/conformance server --url …`; stdio servers need a stdio→HTTP bridge)
   - OpenSSF Scorecard
   - OSV-Scanner / deps.dev
   - Trivy
   - Gitleaks
   - Semgrep CE (check the registry-rules license before using them in a public service)
   - Cisco mcp-scanner (YARA mode)
   - mcplint
   - npm/PyPI provenance attestations

   For the top ~500 only: promptfoo MCP red-team (needs an LLM key). Skip CodeQL (license forbids a hosted service) and mcp-scan (sends tool descriptions to its cloud by default).
3. Later: LLM descriptions via Bedrock (`mcpkg eval-llm` first), domain + HTTPS, semantic search, author claims, PyPI download counts.

## Working style
- User: knows Go, Rust, C++, Python, JS; wants terse answers; use the Mobbin connector for UI design references.
- To save cost: read only the files a task needs, guided by the map above, not the whole repo.
