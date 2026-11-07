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
