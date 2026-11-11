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
| N11 | **High** (Fixed in `1bb546c`) | The deploy gate turned red after the weekly scheduled run. `require_green_ci` (`deploy/aws/lib.sh`) keeps the newest check run per job name. The Monday cron run repeats `go` and `docker` on the same commit and records `bench-gate` as `skipped` (`if: github.event_name != 'schedule'`), with a higher id than the push run's `success`. The gate then exits with `bench-gate: skipped`. This is the state of `a369f02` today (check run 129121956739), so `GATE_SHA=<tag on a369f02> ./deploy/aws/push.sh core` is refused. Any deployed commit is refused again every Monday (worker re-push, redeploy). | In the Python block, ignore skipped runs when picking the newest per name: add `if r.get("conclusion") == "skipped": continue` as the first line of the `for r in ...` loop. A job that only ever skipped still reports "never ran". Add a skipped-after-success case to the gate dry-run test. |
| N12 | Medium | `main` is not protected (`GET /repos/th-anwr/mcpkg/branches/main` returns `protected: false`). Section 13 ruling 7 says to protect it and require `go`, `docker` and `bench-gate`. | Repo Settings, Branches. Manual step. |
| N13 | Info | No release tag exists and `release.yml` has never run, so there are no GHCR images yet. The multi-arch runner and proxy builds under QEMU are untested and may fail on the first run. | Tag `v0.1.0` after N11 is fixed, watch the run, then make the three packages public. |
| N14 | Info | The PAT was pasted in chat again on 2026-11-11. The reviewer did not use it. | Revoke it and create a new fine-grained token after deploy. |

### Verdict

Code quality does not block the first deploy. N11 was the only blocker and is fixed in `1bb546c`: the gate now ignores skipped check runs. Replaying the gate logic on the real check runs of `a369f02` gives red with the old code and green with the new one. CI run 42895401973 on `1bb546c` is green (`go`, `docker`, `bench-gate`) and `lib.sh` is still mode 100755. No automated test covers the gate script, so the first real `push.sh` run is its test.

Next, in order: protect `main` (N12), tag `v0.1.0` on a commit at or after `1bb546c`, let `release.yml` finish and make the three packages public (N13), then follow section 12.7 of `docs/ISSUES.md`.

Still open by decision: N8 (Semgrep license, off by default), issue 43 (log-only), issue 1 (cleartext admin password until TLS), tier-2 gates B6 to B8 before public DNS, and the first real AWS run itself.

## 6. First real deploy (2026-11-11, from Windows)

`v0.1.0` was deployed from a Windows laptop with Git Bash instead of CloudShell: the core box plus two `c7a.xlarge` spot workers. The deploy gate passed, `/healthz` returns `ok`, both workers run security scans, and gVisor is installed. A nightly backup is set up and its first run succeeded.

The deploy scripts assume Linux. The items below broke or needed a manual workaround. None of them affect a CloudShell deploy.

| # | Sev | Finding | Fix |
|---|---|---|---|
| N15 | Medium | `launch.sh` fails with `ParameterNotFound` from Git Bash. MSYS rewrites the SSM name `/aws/service/canonical/...` into a Windows path before it reaches the native `aws.exe`. Other arguments that contain `/...`, such as `DeviceName=/dev/sda1`, are at the same risk. | Add `export MSYS_NO_PATHCONV=1` near the top of `deploy/aws/lib.sh`. Linux ignores it. |
| N16 | Medium | `ensure_key` (`deploy/aws/lib.sh` line 28) writes the key exactly as the Windows AWS CLI prints it, with CRLF line endings. ssh then rejects it: `Load key ... invalid format`. | Pipe the key material through `tr -d '\r'` before writing `$KEY_FILE`. |
| N17 | Medium | The repo has no `.gitattributes`. Git for Windows defaults to `core.autocrlf=true`, so a normal clone converts every script to CRLF. `push.sh` tars the working tree as it is, and `bootstrap.sh` then fails on the box with `$'\r': command not found`. The deploy gate does not catch this, because git still reports a clean tree. | Add `.gitattributes` with `* text=auto eol=lf`. Optionally make `push.sh` refuse to run when any `*.sh` file contains `\r`. |
| N18 | Low | `require_green_ci` (`deploy/aws/lib.sh` line 128) runs `python3`. On Windows that name is the Microsoft Store alias, which exits with "Python was not found", so the gate fails even when Python is installed. | Use `python3` when `python3 -c ''` succeeds, and fall back to `python`. |
| N19 | Low | Two `push.sh` runs in parallel race on `~/.ssh/known_hosts` and print `hostfile_replace_entries failed ... Permission denied`. The warning is harmless, and both pushes succeeded. | Use a known-hosts file under `$STATE_DIR`, or document that worker pushes run one at a time. |
| N20 | Medium | During the first pass, with about 48,600 tools discovered, the light worker hits HTTP 429 from `api.npmjs.org` continuously. `internal/stages/health.go` line 437 requests `/downloads/point/last-week/<pkg>` once per package. The backoff works, but npm download counts lag the rest of the catalog. | Batch unscoped packages: npm's bulk endpoint takes up to 128 comma-separated names per request. Scoped packages still need one request each. |

N15 to N18 could also be closed by stating in `docs/DEPLOY.md` that only CloudShell or Linux is supported. Fixing them in code is about ten lines in `lib.sh` plus one `.gitattributes` file.

**Not verified yet:** smoke test results. When this was written, both workers were still on the security stage. Check that the smoke "works" count in `status.sh` rises over the next few hours.

### 6.1 Fix status (2026-11-11)

Fixed in `8aba0db` and `879312b`. CI run 43004024867 on `879312b` is green: `go` (including the new stage tests and the Postgres end-to-end suite), `docker` and `bench-gate`. Each fix was also exercised on Windows with Git Bash against the real AWS account.

| # | Status | Change | How it was tested |
|---|---|---|---|
| N15 | Fixed | `aws_` in `lib.sh` sets `MSYS_NO_PATHCONV=1` for each AWS CLI call. `llm-gpu-spot.sh` exports it, since it only calls `aws`. | AMI lookup through `aws_` succeeds in Git Bash with no manual setting. |
| N16 | Fixed | `ensure_key` strips `\r` from the key material. | Created a real test key pair through `ensure_key`: 0 carriage returns, `ssh-keygen -y` parses it. The test key pair was deleted. |
| N17 | Fixed | New `.gitattributes` (`* text=auto eol=lf`). New `require_lf_scripts` in `lib.sh`, called by `push.sh`, refuses scripts with CRLF. | A clone with `core.autocrlf=true` now gets LF scripts. An old CRLF clone of `v0.1.0` is refused, with the offending files listed. |
| N18 | Fixed | `require_green_ci` falls back to `python` when `python3` does not run. | The gate passes in a clean `v0.1.0` clone where `python3` is the Microsoft Store alias. |
| N19 | Fixed | `SSH_OPTS` adds `UpdateHostKeys=no`. | 20 parallel connections to the two workers, each round starting from an empty known-hosts file: 6 warnings before, 0 after. |
| N20 | Fixed in code, **not deployed** | Health looks up npm downloads once per batch: one bulk request per 128 unscoped names, one request per scoped name, and single names use the plain endpoint because npm answers them in a different shape. | `TestNpmDownloadsBatch` and `TestNpmDownloadsBatchChunks` cover request counts, the 128-name chunk boundary, scoped names and unknown packages. The e2e npm mock now answers bulk requests. Takes effect only after a new release and a core redeploy. |

| # | Sev | Finding | Status |
|---|---|---|---|
| N21 | Medium | Found while testing N15. With `MSYS_NO_PATHCONV=1` exported for the whole script, Git Bash also stops translating `/c/...` paths for the native `git.exe`. `git -C "$REPO_ROOT"` then fails, and `require_green_ci` silently skips its "checkout is exactly `GATE_SHA` and clean" checks and goes straight to the GitHub check. Today's first deploy ran with that variable exported by hand, so those two checks did not run. The checkout was in fact clean at `v0.1.0`. | Fixed in `879312b` by scoping the variable to `aws` calls. Verified on Windows: the gate now refuses a checkout at the wrong commit and one with an untracked file. |

## 7. After the v0.1.1 release (2026-11-11)

`v0.1.1` runs on the core box. It was redeployed with the fixed scripts and no Windows workarounds, and the database container was not touched. The worker still runs `v0.1.0` images, which is fine, because N20 only changes the health stage on the core.

| # | Sev | Finding | Fix |
|---|---|---|---|
| N20 | Reopened, Medium | Batching did not help. In the 10 minutes after the redeploy, npm 429 warnings rose from 293 to 465, and the share of checked npm tools that got a download count fell from 91–96% to 85%. 57% of npm MCP packages are scoped (12,794 of 22,339), and the bulk endpoint cannot take those, so they still need one request each. The batch now sends its scoped lookups back to back, which is burstier than before. | `httpx.New` already spaces requests per host for `crates.io`, `pypi.org` and `api.github.com`, but not for `api.npmjs.org`. Add a gap for it, for example one request per second, and keep the bulk path for unscoped names. Then measure with the coverage query below. |
| N22 | **High** | Every npm and PyPI smoke install fails under gVisor with `getaddrinfo EAI_AGAIN egress-proxy`. Install containers get `HTTPS_PROXY=http://egress-proxy:3128` on the internal `mcpkg-egress` network, and Docker's embedded DNS does not answer inside gVisor. Verified on a worker with the runner image: `getent hosts egress-proxy` returns 172.18.0.2 under `runc` and fails under `runsc`. By 10:50 UTC, 97 npm and 97 PyPI tools were marked `install_failed`, with no successful installs. Only remote servers pass. These wrong results are written to the catalog and appear on tool pages. | Give the `egress` network a fixed subnet and `egress-proxy` a fixed `ipv4_address` in `deploy/docker-compose.yml`, and have `DockerRunner.InstallArgs` add `--add-host egress-proxy:<that address>`. gVisor reads `/etc/hosts`. Alternatively, set `SANDBOX_PROXY` to the IP literal. Afterwards, re-queue smoke for every `install_failed` tool. Do not switch to `SANDBOX_RUNTIME=` (runc) as a workaround: workers hold the database password, so a container escape could rewrite the catalog. |
| N23 | Medium | Spot worker `i-057f90be528124c7a` (c7a.xlarge, us-east-1a) was reclaimed 38 minutes after launch (10:33:45 UTC), with state reason `Server.SpotInstanceTermination`. Both workers had been placed in us-east-1a. `~/.mcpkg/workers.list` still lists the dead worker. | Let `launch.sh` pick the cheapest zone at launch time: us-east-1f was $0.079 an hour against $0.091 in 1a. Or fall back across c7a, c6a and m7a. Drop dead entries from `workers.list`. |
| N24 | Low | `status.sh` folds `install_failed` into "broken", so "198 broken" read as broken servers, when 194 of them were sandbox install failures. | Report `install_failed` as its own count. |

npm download coverage per 10 minutes, for N20 (run on the core box with `scripts/sql.sh`):

```sql
select to_char(date_trunc('hour', c) + floor(extract(minute from c)/10) * interval '10 min', 'HH24:MI') as utc_10min,
       count(*) as npm_tools_checked,
       round(100.0 * count(*) filter (where (health->>'weeklyDownloads')::bigint > 0) / count(*), 1) as pct_with_downloads
from (select (health->>'checkedAt')::timestamptz as c, health from tools
      where packages @> '[{"registryType":"npm"}]' and health is not null
        and (health->>'checkedAt')::timestamptz > now() - interval '70 min') x
group by 1 order by 1;
```

Deployment problems, with the output that shows each one, are tracked in `docs/DEPLOYMENT_ISSUES.md`.
