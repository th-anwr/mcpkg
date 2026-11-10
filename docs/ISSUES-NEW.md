# ISSUES-NEW — fresh top-down review (2026-11-08)

Scope: architecture, code, security, data, ops — a clean pass over `main`
(commit `e25472f`, all CI green) done independently of the earlier rounds.
This file is new findings + what is still not implemented. Historical fixes
stay in `docs/ISSUES.md`.

## 1. Findings (severity-ordered)

| # | Sev | Where | Finding | Fix |
|---|---|---|---|---|
| N1 | **High** | `internal/web/server.go:537` (`adminOnly`) | CSRF guard is a substring check: `strings.Contains(origin, "://"+r.Host)` accepts `https://example.com.attacker.com` (the attacker origin *contains* `://example.com`), and an **empty Origin/Referer always passes**. A cross-site form POST can approve/reject/hide review items while the admin is logged in. | Parse the Origin with `net/url`, compare `u.Host` (and port) exactly to `r.Host`; refuse POSTs with no Origin. Add a handler test with a suffix-attacker Origin. |
| N2 | Medium | `scripts/make-env.sh` | Writes `.env` (contains `ADMIN_PASSWORD`, `POSTGRES_PASSWORD`, `GITHUB_TOKEN`) **without chmod 600**; CloudShell umask 022 → world-readable. The box copy is installed `0600` by `push.sh`, but the CloudShell/local copy is exposed. Also prints the admin password to the terminal. | `chmod 600 .env` after writing (and `umask 077` at script start). |
| N3 | Medium | `internal/queue/queue.go` `Cleanup` | `Cleanup` deletes only `state='done'` rows; **`dead` jobs accumulate forever** (no TTL, no automatic revive). Over months this table grows unbounded and hides genuinely stuck tools. | Extend `Cleanup` (or the scheduler) to age out `dead` rows (e.g. 30 days) and log/alert on new dead jobs. |
| N4 | Medium | `internal/discover/discover.go` `mergeTools` | `UPDATE identities SET tool_id=$1 WHERE tool_id=$2` can hit a PK conflict when both tools share an identity value; the whole `Upsert` errors and the candidate is counted as an error instead of merging. | `ON CONFLICT DO NOTHING` on the identities move (data is already covered by the strong-key match). |
| N5 | Low | `internal/identity/identity.go` `Slugify`/`ServerKey` | Truncation is byte-based (`s[:120]`, `n[:48]`), which can split a UTF-8 rune and produce an invalid-UTF-8 slug for non-ASCII names. | Truncate at a rune boundary (as `truncate` in stages does). |
| N6 | Low | `internal/bench/bench.go` `rawJSON` | `b[:MaxRawBytes]` can also split a UTF-8 rune in raw output. | Same rune-boundary cut. |
| N7 | Low | `internal/stages/render.go:39-40` | `verified_at` is set even for blocked/hidden tools that reach render via `SkipHeavy` — "verified" can then be read as "safe". | Skip `verified_at` when `status='blocked'`/`visibility='hidden'`, or document the semantics on the page. |
| N8 | Info | `deploy/Dockerfile:31-32` | The app image `pip install`s Semgrep; the CE registry-rules license for a **public hosted service** is still unresolved (HANDOFF / BENCH_GATE open decision 4). | Confirm license or gate semgrep behind an env flag before going public. |
| N9 | Info | ops | Release images must be **public on GHCR** (or boxes logged in) or bootstrap falls back to building on the 2 GB t4g — the OOM risk from ISSUES #16. | Make the three packages public after first `release` run; document in DEPLOY.md (done, keep verifying). |
| N10 | Info | `internal/web/server.go:458-468` | Submission caps key on the last `X-Forwarded-For` hop — correct **only while `web` has no `ports:` mapping** (verified: compose exposes only Caddy). Any future direct exposure of `:8080` makes the cap trivially bypassable. | Add a comment guard + a startup check that warns when `web` is bound to a public interface without Caddy. |

**Verified OK this pass (no change needed):**
- Markdown rendering: no `WithUnsafe()` → raw README HTML never renders; non-http(s) link schemes dropped; `http://` images dropped; CSP img allowlist fixed (issue 24) and tested.
- Enrich LLM: output validated against input (URL grounding, no install commands/markdown, injection-phrase reject, taxonomy-only categories, env_help names must exist), deterministic fallback; LLM never touches scores/install.
- Sandbox: install via allowlist squid (npm/PyPI/GitHub/crates hosts only), run with `--network none`, `--read-only`, `--cap-drop ALL`, pid/mem/cpu limits, optional gVisor; process mode is dev-only with scrubbed env and explicit warning.
- Queue: `FOR UPDATE SKIP LOCKED` claims, leases + reaper, backoff 1m→6h, dead-lettering, one active job per (stage, tool).
- Dedupe: strong/weak identity keys, advisory lock serializing merges, source precedence recomposition.
- Submissions: 8 KiB body cap, 500-char URL, per-client + global hourly caps, 30 s resolve timeout.
- Gate loop: tier-1 gates green per commit, report committed by a retry-safe bot, `push.sh` refuses without matching `GATE_SHA`.
- `web` compose service has no `ports:` (issue 26 satisfied); Caddy is the only edge.

## 2. Not implemented (consolidated — no changes made here)

**UI (ISSUES §5)** — only the Benchmarks tab exists. Missing: header version/visibility/publish-age, Readme-first layout, Code tab, Dependencies tab, Dependents tab, Versions tab, downloads sparkline, maintainers/verified-owner block, `metadata` stage that fills them, Mobbin UI pass.

**Features**
1. GitHub sign-in, claims, owner edits, sessions/users tables (ISSUES §3) — admin is basic-auth only.
2. Semantic search; Code tab (read-only file browser).
3. Benchmarks remainder: CLI scanners (gitleaks/trivy/semgrep/mcplint) wired to `BENCH_REPO_DIR`, stdio conformance bridge, promptfoo red-team (top 500).
4. Tier-2 soak gates B6 (perf), B7 (cost), B8 (teardown/restore drill) — required before public DNS.
5. B4 (page/API truth) asserted inside `TestGate` — currently implemented but only indirectly tested.
6. LLM descriptions via Bedrock (optional; README fallback works).
7. PyPI download counts, provenance badges, author claim re-verification.
8. Domain + HTTPS (Caddy TLS); site is HTTP on IP until `DOMAIN` set.

**Ops/governance**
9. First real AWS deploy — never executed; ISSUES #2 risks (apt mirrors, docker tgz URL, arm64) still pending.
10. Branch protection on `main` — unverifiable with the PAT used; confirm in settings.
11. `BENCH_ENABLED` still `false` everywhere — benchmarks won't run until flipped on workers.
12. Rotate the PAT used for this session's commits (shared in chat).
13. Dead-job aging (N3), `.env` chmod (N2), CSRF fix (N1).

## 3. Review boundaries (be honest about coverage)

- Re-read in depth this pass: `app`, `config`, `db`, `queue`, `discover`, `identity`,
  `launch`, `model`, `score`, `stages/{common,health,security,smoke,enrich,render,bench}`,
  `scan`, `sandbox`, `mcpclient`, `httpx`, `llm`, `store`, `submit`, `bench`,
  `web/{server,api,markdown}`, `pipeline`, `pipeline.json`, migrations,
  `deploy/{Dockerfile,docker-compose.yml,push.sh,bootstrap.sh,squid.conf}`,
  `scripts/make-env.sh`, workflows, docs.
- Not re-read this pass (covered by earlier rounds + tests, recheck if paranoid):
  `internal/installgen`, `internal/finder`, `internal/web/funcs.go`, static assets,
  `scripts/{backup,batch,compose,dev,mcpkg,progress,sql,status}.sh`,
  `deploy/aws/{launch,lib,ssh,teardown}.sh`, `docs/ARCHITECTURE.md` (no drift diff
  run against code yet).
- Suggested for the Opus pass: run a drift diff between `docs/ARCHITECTURE.md` and
  the code, re-verify N1's fix with a negative test, and decide the Semgrep
  license question (N8) before public launch.

## 4. Status after the Opus pass (2026-11-08)

Each finding was checked against the code before any change. The negative tests fail on the old code.

| # | Verdict | Change |
|---|---|---|
| N1 | **Confirmed** (High) | `sameOrigin()` parses Origin (Referer as fallback) and requires its host to equal `r.Host` exactly. POSTs with neither header are refused. `TestAdminCSRF`: suffix attacker, other site, other port, missing headers, `null` origin. Against the old guard it lets the attacker cases through (204). |
| N2 | Partly | `umask 077` already made a new `.env` mode 600, but an existing `.env` kept its old mode on re-run. `make-env.sh` now runs `chmod 600 .env`. Printing the admin password is intended (it is shown once so you can save it). |
| N3 | Confirmed (Medium) | `Queue.Cleanup` also deletes dead jobs older than `queue.DeadTTL` (30 days). Recent dead jobs stay for `status.sh`. Covered by `TestCleanupAgesOutDeadJobs`. |
| N4 | **Not an issue** | The `identities` primary key is `(kind, value)`. `UPDATE identities SET tool_id=…` never changes key columns, so it cannot conflict. |
| N5 | **Not an issue** | `Slugify` and `ServerKey` first replace everything outside `[a-z0-9.-]` / `[a-z0-9-]`, so the string is ASCII before it is truncated. |
| N6 | Fixed earlier | `rawJSON` no longer cuts output; oversized output becomes a `{"truncated":true}` marker (ISSUES #38). |
| N7 | Not an issue | Stopped/blocked tools never reach `render` (`Stop` has no next stage). `SkipHeavy` covers archived or inactive repos, which are not blocked. `verified_at` means "last full check", and the page labels it "Checked N ago". |
| N8 | Open (pre-launch) | Semgrep is only installed with `--build-arg WITH_SEMGREP=true` (default false), so the default image has none. Resolve the license question before enabling it. |
| N9 | Done | DEPLOY.md "Before you start" step 5 makes the three GHCR packages public. |
| N10 | Accepted | Verified: `web` has no `ports:`. |

**Correction to "Verified OK" above:** the gate no longer commits a report or matches it against `GATE_SHA`. `push.sh` now requires green `go`, `docker` and `bench-gate` check runs on GitHub, plus a clean checkout at that commit (ISSUES #29, #31).

## 5. Reviewer cross-check (2026-11-11, commit `a369f02`)

CI on `a369f02`: `go`, `docker` and `bench-gate` are green on the push run (42411658586). The weekly run (42605974504) is green too, and its `e2e-network` job passed with real npm/PyPI installs.

Verified against the code, not the commit messages:

- N1: `sameOrigin` parses Origin (Referer as fallback) and compares the host exactly; POSTs with neither header are refused. `TestAdminCSRF` exists.
- N2: `make-env.sh` has `umask 077` and `chmod 600 .env`.
- N3: `Queue.Cleanup` ages out dead jobs after `DeadTTL` (30 days). `TestCleanupAgesOutDeadJobs` is in `internal/e2e/db_test.go`.
- Issue 24: CSP concatenation is correct and `TestSecurityHeaders` covers it.
- Issue 25: `release.yml` builds all three images for amd64 and arm64.
- Issue 28: every `*.sh` in the tree is mode 100755.
- Issue 26 / N10: `web` has no `ports:`; only `db` and `caddy` publish ports.
- Issue 10: `golangci-lint`, `gosec` and the weekly `e2e-network` job are in `ci.yml`.

### New findings

| # | Sev | Finding | Fix |
|---|---|---|---|
| N11 | **High** | The deploy gate turns red after the weekly scheduled run. `require_green_ci` (`deploy/aws/lib.sh`) keeps the newest check run per job name. The Monday cron run repeats `go` and `docker` on the same commit and records `bench-gate` as `skipped` (`if: github.event_name != 'schedule'`), with a higher id than the push run's `success`. The gate then exits with `bench-gate: skipped`. This is the state of `a369f02` today (check run 129121956739), so `GATE_SHA=<tag on a369f02> ./deploy/aws/push.sh core` is refused. Any deployed commit is refused again every Monday (worker re-push, redeploy). | In the Python block, ignore skipped runs when picking the newest per name: add `if r.get("conclusion") == "skipped": continue` as the first line of the `for r in ...` loop. A job that only ever skipped still reports "never ran". Add a skipped-after-success case to the gate dry-run test. |
| N12 | Medium | `main` is not protected (`GET /repos/th-anwr/mcpkg/branches/main` returns `protected: false`). Section 13 ruling 7 says to protect it and require `go`, `docker` and `bench-gate`. | Repo Settings, Branches. Manual step. |
| N13 | Info | No release tag exists and `release.yml` has never run, so there are no GHCR images yet. The multi-arch runner and proxy builds under QEMU are untested and may fail on the first run. | Tag `v0.1.0` after N11 is fixed, watch the run, then make the three packages public. |
| N14 | Info | The PAT was pasted in chat again on 2026-11-11. The reviewer did not use it. | Revoke it and create a new fine-grained token after deploy. |

### Verdict

Code quality does not block the first deploy. N11 does: `push.sh` will refuse the current commit. Fix N11, tag, let `release.yml` finish, then follow section 12.7 of `docs/ISSUES.md`.

Still open by decision: N8 (Semgrep license, off by default), issue 43 (log-only), issue 1 (cleartext admin password until TLS), tier-2 gates B6 to B8 before public DNS, and the first real AWS run itself.
