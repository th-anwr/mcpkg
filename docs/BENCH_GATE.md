# Benchmark gate (mcpkg)

Status: **tier 1 implemented** (B0–B3, B5; `internal/bench/gate_test.go`). Tier 2 (B6–B8) not built. Blocks deploy.
Version 0.1 — numeric thresholds are provisional and freeze after the first baseline run.

## Why this document exists

"Production benchmark" is currently undefined. A benchmark with no numeric pass/fail
cannot block a deploy, and `docs/ISSUES.md` #2 says the first real build/run *is* the
deploy. This file defines the gate before any benchmark code is written, so the stage is
built to satisfy tests rather than tests being written to match the stage.

## Two tiers

| Tier | When it runs | Blocks |
|---|---|---|
| **T1 — pre-deploy** | CI job `bench-gate` on every commit to `main`, over a 200-tool fixture set | `deploy/aws/push.sh` (any push to a real box) |
| **T2 — soak** | Once, on a prod-shaped stack (1 core + 2 workers), torn down after | Public launch: `DOMAIN`, DNS, traffic |

Reason for the split: T2 measures performance and cost, which cannot be measured without
a prod-shaped run. A staging soak is *not* the production deploy; it is the evidence T2
needs.

## Gates

| ID | Gate | Pass condition | Tier | Evidence |
|---|---|---|---|---|
| **B0** | Contract + provenance | 100% of benchmark rows validate schema v1; every row carries scanner, scanner version, image digest, `ran_at` (UTC), `duration_ms`, `exit_code`, `verdict`, raw-output ref. **0** rows without provenance. | T1 | schema validator over fixture output |
| **B1** | Coverage, no silent gaps | ≥ 99.5% of tools reaching `enrich` get a terminal bench state (`pass\|fail\|warn\|skip\|error`); every non-result has an enum reason code; unknown count reported in the report. 0 tools dropped silently. | T1 | `bench-gate` run on 200-tool fixture |
| **B2** | Known-answer set | 12 pinned tools land their exact expected verdict: 3 clean → listed (score ≥ B), 3 with MAL/OSV advisory → hidden, 3 with static findings → review (not hidden, not clean), 3 dead/404 → unavailable (not `pass`). 12/12. | T1 | `go test ./internal/bench -run TestKnownAnswers` |
| **B3** | Determinism + idempotency | Two consecutive runs on identical inputs produce identical verdicts and scores (timestamps excepted). Re-running a completed tool adds 0 rows. 0 orphan rows. | T1 | same test, run twice, JSON diff |
| **B4** | Page + API truth | For 3 fixture slugs, the Benchmarks tab renders each scanner's verdict, raw output, scanner version and run date; `/api` returns the same JSON; 0 verdicts displayed without provenance; empty state states the skip reason. | T1 | `internal/web` handler test + golden HTML |
| **B5** | Safety invariants | Scanner containers run `--network none`; install egress only via allowlist proxy; LLM output never writes verdict/score/install; static findings never auto-hide; only `MAL-*`/takedown hide. An adversarial README that tells the agent to hide the tool changes nothing. | T1 | unit tests + `TestAdversarialReadme` |
| **B6** | Performance | Bench stage p95 ≤ 90 s/tool (heavy scanners), ≤ 15 s/tool (light); whole pipeline p95 ≤ 6 min/tool at concurrency 4; 1,000-tool reference run ≤ 4 h wall on 1 core + 2 workers. *Freeze after baseline.* | T2 | soak run metrics |
| **B7** | Cost | 30-day projected AWS spend ≤ the $114 credit envelope; bench stage ≤ 40% of worker compute; 0 idle workers after the first pass. | T2 | Cost Explorer + run ledger |
| **B8** | Ops + rollback | `deploy/aws/teardown.sh` removes workers and leaves core intact; backup + restore drill restores benchmark rows; `DEPLOY.md` documents the drill. | T2 | executed drill, written up |

## Gate report and enforcement

- CI job `bench-gate` runs the tier-1 gates on every push and pull request and fails on any
  red gate. `TestGate` writes `bench/gate.json` (`{ "sha", "tier": 1, "gates": [{ "id", "pass", "value", "threshold" }] }`),
  kept as the `bench-gate-report` artifact of that run. It is not committed: a bot commit per push
  breaks branch protection, races other pushes, and the report always described the previous commit.
- `deploy/aws/push.sh` deploys only when `GATE_SHA=<tag|sha>` is a commit whose `go`, `docker` and
  `bench-gate` check runs are green on GitHub (`require_green_ci` in `deploy/aws/lib.sh`, public
  check-runs API). With a git checkout it also refuses a working tree that is not exactly that
  commit, because `push.sh` uploads the working tree. Break-glass: `GATE_OVERRIDE="<reason>"`.
- With branch protection on `main`, make `go`, `docker` and `bench-gate` required status checks.

## Stage contract (what "writing it" must satisfy)

- `internal/bench`: scanner adapters behind one interface; each pinned by name, version
  and image digest; bounded timeout; retry ≤ 2; raw output capped at 256 KiB.
- Tables: `tool_benchmarks(tool_id, scanner, scanner_version, image_digest, ran_at,
  duration_ms, exit_code, verdict, raw jsonb, run_id)` and
  `bench_runs(id, sha, started_at, finished_at, gate jsonb)`.
- Enums: `verdict = pass|fail|warn|skip|error`; `skip_reason` enumerated, never free text.
- Feature flag `BENCH_ENABLED`; the tab stays hidden until B4 is green.
- Scanner order, cheap → expensive: conformance, scorecard, osv, trivy, gitleaks,
  semgrep, mcp-scanner, mcplint. promptfoo only for the top 500, behind a second flag.
- Built today: `scorecard` (API), `osv` (API), `mcp-handshake` (mcpkg's own handshake against
  remote endpoints, deliberately **not** called "conformance": it is not the official
  `@modelcontextprotocol/conformance` suite). `gitleaks`, `trivy`, `semgrep`, `mcplint` are
  placeholders that report `skip=scanner-unavailable` and are off by default.
- Skip reasons: `no-repo`, `no-endpoint`, `no-package`, `scanner-unavailable`, `not-applicable`,
  `timeout`, `no-data` (upstream has no result, e.g. repo not in the Scorecard dataset),
  `auth-required`, `needs-config` (templated URL or declared headers). "Not measured" is a skip,
  never `fail`; an upstream outage is `error`, never `fail`.

## Open decisions

1. **Scorecard**: run it ourselves, or read it from deps.dev (one keyless call, but stale)?
2. **stdio servers + conformance**: bridge to HTTP inside the sandbox, or skip with a
   reason code?
3. **Raw output retention**: Postgres `jsonb` or object storage? 256 KiB × tools ×
   scanners is the biggest unknown data cost.
4. **Semgrep CE registry rules** for a public hosted service — license check still open
   (already flagged in HANDOFF).
