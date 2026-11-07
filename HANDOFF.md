# mcpkg handoff (read this first; open other files only when a task needs them)

## What it is
npm-style catalog of open-source MCP servers. Every server is discovered, checked (health → security → smoke → enrich → render), scored A–F, and gets a page with exact install steps for people and agents. Working name "mcpkg" (domain not chosen; mcp.net likely taken).

## Status (2026-11-08)
- Code complete, all tests pass: unit, Postgres, end-to-end with real npm/PyPI installs, plus golangci-lint, gosec, and the tier-1 benchmark gate (B0–B3, B5) in CI.
- **Not deployed yet.** Plan: AWS CloudShell following `docs/DEPLOY.md` (git clone + release tag). `push.sh` needs `GATE_SHA=<tag|sha>` and deploys only if GitHub shows `go`, `docker` and `bench-gate` green for that commit and the checkout is exactly that commit (`require_green_ci` in `deploy/aws/lib.sh`).
- The AWS scripts were only dry-run against a fake AWS CLI. Docker images have never been built on a real box: the build sandbox blocked image registries.

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
# DB tests (e2e and internal/bench TestGate) reset the same schema: with TEST_DATABASE_URL set, run `go test -p 1 ./...`
```

## Next tasks (agreed or offered)
1. Deploy (`docs/DEPLOY.md`), then fix whatever breaks on the first real box.
2. **Benchmarks: tier-1 done, off by default (`BENCH_ENABLED=false`).** `internal/bench` (scorecard, osv, mcp-handshake; placeholder adapters for gitleaks/trivy/semgrep/mcplint), `tool_benchmarks`/`bench_runs` tables, Benchmarks tab + `/api/bench/{slug}`, and the `bench-gate` CI job enforcing B0–B3+B5 per commit (`docs/BENCH_GATE.md`). Remaining:
   - Wire the CLI scanners to `BENCH_REPO_DIR` checkouts (they currently report `skip=scanner-unavailable`).
   - stdio conformance bridge, promptfoo red-team for the top 500.
   - **Tier-2 soak before public launch**: B6 perf, B7 cost, B8 teardown/restore drill.
3. Later: LLM descriptions via Bedrock (`mcpkg eval-llm` first), domain + HTTPS, GitHub sign-in + claims, metadata stage + npm-style tabs, semantic search, author claims, PyPI download counts.

## Working style
- User: knows Go, Rust, C++, Python, JS; wants terse answers; use the Mobbin connector for UI design references.
- To save cost: read only the files a task needs, guided by the map above, not the whole repo.
