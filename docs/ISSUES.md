# mcpkg: issues, fixes, and the npm-style page plan

Date: 2026-11-07. Scope: what was found reviewing `pipeline.json`, `deploy/`, `cmd/mcpkg`, `internal/web`, `internal/db/migrations`, plus the product gaps raised (owner sign-in, third-party health data, npm-style layout).

## 1. Issues found

| # | Severity | Issue | Fix |
|---|---|---|---|
| 1 | Low | `/admin/*` is behind HTTP Basic auth (`ADMIN_PASSWORD`, constant-time compare, same-origin CSRF guard; 403 when unset). Corrected from an earlier "no auth" claim. Remaining risk: one shared password, sent in clear until TLS is on. | Set `ADMIN_PASSWORD` via make-env.sh (already does), enable TLS first. Long term: GitHub OAuth + `ADMIN_GITHUB_LOGINS` (section 3). |
| 2 | **High** | Docker images never built; AWS scripts only dry-run. First real run will hit surprises (apt mirrors, docker static tgz URL, osv-scanner download, arch on t4g = arm64). | Build locally first: `docker build -f deploy/Dockerfile .` and `sandbox/runner.Dockerfile`. Pin `DOCKER_CLI_VERSION` and osv-scanner version; use `TARGETARCH` build arg for arm64 vs amd64. |
| 3 | Med | `LOG_DIR` default `logs/` in containers → `/app/logs`, ephemeral, and duplicates stderr that Docker already rotates (json-file 20m×3). | Set `LOG_DIR=-` in `.env` for compose, or add a named volume `logs:/logs` + `LOG_DIR=/logs`. Keep file logs for bare-metal/dev. |
| 4 | Med | `worker-heavy` declares its own `volumes:`, which replaces any anchor-level volumes (YAML merge is shallow). | Any shared volume must be repeated in `worker-heavy`. |
| 5 | Med | App container runs as root; `worker-heavy` mounts `/var/run/docker.sock` (root-equivalent on host). | Accepted for v1 (sandbox needs the daemon). Add `USER` to `web`/`scheduler`/`worker-light` via a second stage, or run heavy workers only on throwaway spot boxes (already the plan). Document it. |
| 6 | Med | Caddy `DOMAIN` defaults to `localhost` → HTTP only; HSTS header is emitted even on HTTP. | Set `DOMAIN` before first boot; gate the HSTS header on `{$DOMAIN} != localhost` or drop it until TLS is on. |
| 7 | Med | `DB_BIND` default `127.0.0.1`; for spot workers it must be the private IP and the SG must allow only the VPC CIDR on 5432. | Document in `DEPLOY.md`; `launch.sh` should set `DB_BIND` from the instance's private IPv4 metadata. |
| 8 | Low | `pipeline.json` hook uses `python3`; image installs it, fine, but hooks run with the app's env and no timeout isolation beyond `timeout_sec`. | OK as is. Add `"on_error": "fail"` for any hook that gates listing. |
| 9 | Low | No `/submit` abuse control visible (rate limit, captcha, dedupe on URL). | Per-IP token bucket in `httpx`/middleware, dedupe on normalized repo URL, cap queue size. |
| 10 | Low | No CI in repo (no `.github/workflows` seen). Tests only run locally. | Add workflow: `go vet`, `go test ./...`, `govulncheck`, `golangci-lint`, `docker build` (no push) on PR. |
| 11 | Low | Scores shown without a "last checked" freshness signal on the card. | Show `checked_at` + stale badge if > `recheck.health_every_days`. |
| 12 | Info | Fixed today: `internal/logx` added, `main.go` wired, `logs/` ignored. Not compiled (no Go on the review machine). | Run `go build ./... && go test ./...` before deploy. |

## 2. Rules and flow to follow

**Branching**
- `main` protected: PR required, CI green, 1 review (self-review OK while solo, but no direct pushes).
- Branch names `feat/…`, `fix/…`, `ops/…`. Squash merge. Conventional commit titles.

**CI gates (every PR)**
1. `gofmt -l` empty, `go vet`, `golangci-lint`.
2. `go test ./...` and the Postgres e2e against a service container.
3. `govulncheck ./...`; `gosec` for `internal/sandbox`, `internal/launch`, `internal/web`.
4. `docker build` both images for `linux/amd64,linux/arm64`.
5. Weekly `E2E_NETWORK=1` run on a schedule, not per PR.

**Deploy flow**
1. Tag `vX.Y.Z` → CI builds and pushes images to ECR/GHCR with the tag and `sha`.
2. `deploy/aws/push.sh` pulls by tag; never `latest` in prod compose.
3. Migrations run via the `migrate` service before `web` starts (already so). Rule: migrations are forward-only, additive; a destructive change ships two releases later.
4. `scripts/backup` runs nightly via cron on the core box; test a restore once before launch.

**Data integrity rules (already true, keep them written down)**
- Scores and install commands are computed only by `internal/score` / `internal/installgen`. No human or LLM edit path to them.
- Every check result row keeps `tool_version`, `ran_at`, `runner_image` so the Benchmarks tab can show provenance.
- Any page edit by an owner is a new row in `page_edits` (who, when, diff), never an in-place update.

## 3. Missing: GitHub sign-in and repo claims

Not present today (`server.go` has no auth, no `users`/`sessions`/`claims` tables). Needed so maintainers can (a) see that their repo is listed, (b) correct the human-written parts of the page, (c) get notified on score drops.

**Flow**
1. `GET /auth/github` → redirect to GitHub OAuth (scopes: `read:user`, `read:org`; no repo write scope).
2. `GET /auth/github/callback` → exchange code, fetch `/user`, create `users` row, set HttpOnly SameSite=Lax session cookie (random 32-byte id, 30-day expiry, stored in `sessions`).
3. `POST /tool/{slug}/claim` → server calls GitHub `GET /repos/{owner}/{repo}` **with the user's token** and requires `permissions.push || permissions.admin`. For org-owned repos also accept org `admin` membership. Store `claims(tool_id, user_id, role, verified_at)`. Re-verify every 30 days.
4. Claimed pages show "Maintained by @login · verified" in the sidebar.

**What a verified owner may edit** (and nothing else)
- Short description, homepage, categories/tags, env var documentation, "recommended client" notes, hide/unhide a deprecated version.
- Request a re-check (rate-limited, 1/hour).
- Not editable: scores, smoke/security/benchmark results, install commands (generated), download counts.

**Edit flow**: form → `page_edits` row (status `pending`) → applied immediately if the user is a verified owner, else to `review_queue`. Both keep history; a "Revert" admin action exists. Public pages show "edited by owner on <date>".

**Tables**
```sql
users(id, github_id bigint unique, login, avatar_url, created_at)
sessions(id text pk, user_id, expires_at, created_at)
claims(tool_id, user_id, role text, verified_at, primary key(tool_id,user_id))
page_edits(id, tool_id, user_id, field, old_value, new_value, status, created_at, applied_at)
```

**Admin**: `ADMIN_GITHUB_LOGINS=th-anwr` env; admin routes require a session whose login is in that list. Replaces the current open `/admin/*`.

## 4. npm's "health" buttons: what they are, can we use them

| npm button | Behind it | Open? | Use in mcpkg? |
|---|---|---|---|
| Analyze security with Socket | socket.dev (proprietary SaaS; free API tier needs key; ToS limits redistribution) | No | Link only, optional. Our own security stage already covers static checks. |
| Check bundle size | bundlephobia.com (MIT, self-hostable, npm-only, public API without SLA) | Yes | Low value for MCP servers (not browser bundles). Skip; maybe "install size" from our smoke stage instead. |
| View package health | Snyk Advisor (proprietary score, no public API) | No | Skip. Our Trust score is the equivalent. |
| Explore dependencies | npmgraph.js.org (MIT, client-side) | Yes | Link only ("open in npmgraph"). |

**Open sources we should use instead (all free, documented, multi-ecosystem where noted)**

| Source | Gives | Ecosystems | Notes |
|---|---|---|---|
| deps.dev API (Google) | OpenSSF Scorecard, OSV vulns, licenses, dependency graph, dependents count | npm, PyPI, crates, Go, Maven | One call replaces running Scorecard ourselves for most repos. Rate limits generous, no key. |
| OSV.dev API | Vulnerabilities per package/version | all | Already planned via osv-scanner; the API is cheaper for the metadata stage. |
| npm registry + `api.npmjs.org/downloads/range` | Versions, maintainers, dist-tags, provenance attestations, daily downloads (sparkline) | npm | Public, no key. |
| PyPI JSON API + pypistats.org API | Versions, maintainers, downloads (last 30d by day) | PyPI | pypistats is public; BigQuery for history if needed. |
| crates.io API | Versions, owners, downloads | crates | Requires a descriptive User-Agent (we have `cfg.UserAgent`). |
| Sigstore / npm provenance, PyPI attestations | Build provenance verified badge | npm, PyPI | Already on the benchmark list. |
| ecosyste.ms API | Dependents, repo metadata across ecosystems, open | all | Good fallback for "Dependents" tab. |
| libraries.io | Similar, needs API key, stricter limits | all | Fallback only. |

Rule: cache every third-party response in Postgres with `fetched_at`; render from the DB, never live. Show the source and fetch time next to each number.

## 5. npm-style page: current vs target

**Current `tool.html`**: single long page. Tabs nav exists but sections are: tools list → values you need → install methods → "For AI agents" → score card → smoke test → security scan → repository health → check history. Sidebar: Install, Trust score, Can (capabilities), README badge.

**Target** (same as npm's package page; swap their vendor buttons for our checks)

```
┌ name · version · public · published N ago ───────────────────────────────┐
│ [Readme] [Code] [Dependencies N] [Dependents N] [Versions N] [Benchmarks] │
├───────────────────────────────────────────────┬──────────────────────────┤
│ tab body                                      │ Install  ┌ copy ─┐       │
│   Readme: rendered README (goldmark, same)    │ Repository               │
│   Code: file tree + file viewer (read-only)   │ Homepage                 │
│   Dependencies: table from lockfile/manifest  │ Weekly downloads + spark │
│   Dependents: from deps.dev / ecosyste.ms     │ Version | License        │
│   Versions: table, dist-tags, dates, dl/wk    │ Last publish             │
│   Benchmarks: our stages + tools, raw results,│ Maintainers (+verified)  │
│     tool version, run date (planned stage)    │ ── Trust score  A 87/100 │
│                                               │ [Security scan]          │
│                                               │ [Smoke test · verified]  │
│                                               │ [Scorecard · OSV]        │
│                                               │ [MCP conformance]        │
│                                               │ [Report a problem]       │
│                                               │ README badge             │
└───────────────────────────────────────────────┴──────────────────────────┘
```

**Differences to implement**

| Area | npm | mcpkg now | Change |
|---|---|---|---|
| Header | name, version, visibility, publish age | name only | Add version/age from registry metadata. |
| Tabs | Readme, Code, Dependencies, Dependents, Versions | sections, no real tabs | Real tabs, one body at a time, URL hash `#dependencies`. "Benchmarks" tab is new. |
| Readme | full README | tools list first | README first; move tools + "Values you need" + install methods into Readme tab below README, or into an "Install" tab if README is long. |
| Code tab | file browser | none | Phase 2; read-only tree from GitHub API with caching. Low priority. |
| Dependencies | from package.json | none | New: parse manifest/lockfile in enrich stage, store `tool_deps`. |
| Dependents | npm internal | none | New: deps.dev / ecosyste.ms count + top list. |
| Versions | registry | none | New: `tool_versions` table, filled by crawlers. |
| Downloads | weekly + sparkline | `downloads` column only | New: `downloads_daily`, render SVG sparkline server-side (no JS lib). |
| Maintainers | npm users | none | Registry maintainers + GitHub-verified owners from section 3. |
| Vendor buttons | Socket, Bundlephobia, Snyk, npmgraph | none | Replace with our check links (score card stays on top). |
| Trust score | none (Snyk link) | sidebar card | Keep; this is the differentiator. |
| "For AI agents" | none | section | Keep as a small sidebar block linking `install.md` / `manifest.json`. |

**Data/schema additions**: `tool_versions(tool_id, version, published_at, dist_tags, provenance)`, `tool_deps(tool_id, version, name, range, dev bool)`, `downloads_daily(tool_id, day, count, source)`, `dependents(tool_id, count, fetched_at, sample jsonb)`. Fill from a new light stage `metadata` (after `health`, before `enrich`) so heavy workers are not involved.

**UI**: use the Mobbin connector for package-page references before styling; keep `style.css` tokens, add tab styles only.

## 6. Order of work

1. Auth + admin lock (issue 1, section 3 minimal: sign-in, allowlist admin, no claims yet).
2. Build images, fix Dockerfile for arm64, deploy (issues 2–7).
3. `metadata` stage + Versions/Downloads/Dependencies tabs (section 5).
4. Claims + owner edits.
5. Benchmarks stage and tab.
6. Code tab, Dependents, semantic search.

## 7. Review round 2 (2026-11-07, evening)

Added `.github/workflows/ci.yml` (gofmt, vet, build, unit tests, Postgres e2e, govulncheck, three Docker image builds) so the suite runs on GitHub. First run: https://github.com/th-anwr/mcpkg/actions/runs/42384515834

| # | Severity | Issue | Fix |
|---|---|---|---|
| 13 | Fixed | GitHub Actions billing lock. Resolved by the owner on 2026-11-07; runs now start. | None. |
| 14 | Med | Logging exists only in `cmd/mcpkg`, `app`, `discover`, the pipeline runner. 27 of 44 non-test files use no slog: all five stages, sandbox (docker/process), httpx, queue, sources, mcpclient, store, submit. A stuck smoke test or a rate-limited crawler is invisible. | Propagate `internal/logx`: `stages.Deps.Log` already exists, use `deps.Log.With("stage", name, "tool", slug, "job", id)` in each stage. httpx: Debug per request (host, status, ms, attempt), Warn on rate limit. sandbox: Info on container start/stop (image, runtime, exit code, duration). queue: Info on lease, retry, dead-letter. Rule: an error is logged once, at the runner, with those attrs; stages log progress and external calls only. |
| 15 | Med | `DATABASE_URL` default is `postgres:postgres@127.0.0.1` with `sslmode=disable`. Fine for dev; silent if `.env` loses the line. | In `config.Load`, refuse to start when `SITE_URL` is https and `DATABASE_URL` equals the default. |
| 16 | Med | `bootstrap.sh` runs `curl get.docker.com | sh` and builds all images on the t4g.small (2 GB RAM + 2 GB swap). Go build + apt + osv-scanner download will be slow and may OOM. | Build in CI, push to GHCR, pull by tag on the box (section 2). Use the distro Docker package or pin the script version. |
| 17 | Fixed | `ubuntu-latest` pinned to `ubuntu-24.04` in ci.yml (commit 9656a6a). | None. |
| 18 | Low | CSP `img-src https:` lets README images load from any host (tracking pixels in READMEs). | Proxy README images or allow-list `raw.githubusercontent.com`, `img.shields.io`, registry CDNs. |
| 19 | Low | `make-env.sh` writes helper vars `ADMIN_PASS`/`PG_PASS` next to the real ones. | Cosmetic; drop or comment them. |
| 20 | Info | Good as is: `http.Server` has Read/Write/ReadHeader timeouts; security headers set; httpx has per-host rate limit, Retry-After, body caps; e2e tests skip without `TEST_DATABASE_URL`; admin has CSRF guard. | None. |

**Suite status**: see section 8.

## 8. CI results (2026-11-07, run 42385463339)

https://github.com/th-anwr/mcpkg/actions/runs/42385463339

| Job | Step | Result |
|---|---|---|
| go | gofmt, go vet, go build | pass |
| go | unit tests (`go test ./...`) | pass |
| go | Postgres e2e (`internal/e2e` with `TEST_DATABASE_URL`) | pass |
| go | govulncheck | pass (advisory) |
| docker | `deploy/Dockerfile` app image | pass |
| docker | `sandbox/runner.Dockerfile` | pass |
| docker | `sandbox/proxy.Dockerfile` | **fail** (issue 21) |

Earlier runs failed on a type mismatch in `internal/logx/log.go` (int64 vs int in the `Open` call); fixed in commit 992702d. Go code compiles and the whole test suite is green on GitHub.

| # | Severity | Issue | Fix |
|---|---|---|---|
| 21 | **High** | Egress proxy image never built: squid rejects `sandbox/squid.conf` with `'registry.npmjs.org' is a subdomain of '.npmjs.org'` (overlapping entries in the `registries` ACL). Without this image no smoke test can install anything in production. | In `sandbox/squid.conf` line 11, remove `registry.npmjs.org` (already covered by `.npmjs.org`). CI will confirm. |
| 22 | Med | The same allowlist has no crates.io hosts, but `internal/sources/crates.go` discovers Rust servers. `cargo install` will fail in the sandbox for every one of them. | Add `acl registries dstdomain crates.io index.crates.io static.crates.io`. Also consider `cdn.jsdelivr.net` if any install path uses it. |
| 23 | Low | `actions/checkout@v4`, `setup-go@v5`, `setup-buildx-action@v3` target Node 20, now deprecated on runners (warning only). | Bump to the current majors when convenient. |

**Rule**: `squid -k parse` already runs in the proxy Dockerfile, which is what caught this. Keep it, and add the three Docker builds to the required checks on `main` once branch protection is on.

## 9. Fix round (2026-11-07, night)

| # | Status | What changed |
|---|---|---|
| 1 | Open (planned) | Basic auth stays until GitHub sign-in (section 3, order of work step 1). `DEPLOY.md` now warns that the password is sent in clear text until TLS. |
| 2 | Fixed | `deploy/Dockerfile` cross-compiles Go on `$BUILDPLATFORM` for `$TARGETARCH`. CI builds the app image for amd64 + arm64. Docker CLI and osv-scanner were already pinned. |
| 3 | Fixed | Compose sets `LOG_DIR=-` for every app service (the `*env` anchor). The image defaults to `LOG_DIR=-`. `make-env.sh` and `.env.example` include it. |
| 4 | Fixed | Services that set their own `environment:` merge `*env`. `worker-heavy` documents that its `volumes:` replaces the anchor's. |
| 5 | Fixed | The image runs as uid 10001. Only `worker-heavy` runs as root (`user: root`, because it needs the Docker socket), and only on spot workers. |
| 6 | Fixed | Caddy sends HSTS only on HTTPS responses (`@https protocol https`). The compose `DOMAIN` default is now `:80`. |
| 7 | Already done | `bootstrap.sh` sets `DB_BIND` from IMDS `local-ipv4`, and the security group allows 5432 only from the group. Now documented in `DEPLOY.md`. |
| 8 | Fixed | The license hook uses `"on_error": "stop"`. The valid values are `continue` and `stop`, not `fail`. |
| 9 | Fixed | Already in place: a cap of 10 per client per hour, and dedupe via identity merge. New: a site-wide cap of 300 per hour, an 8 KiB form limit, and a 500-character URL limit. The client key now uses the last `X-Forwarded-For` hop (set by Caddy), because earlier hops can be forged. |
| 10 | Partly | `ci.yml` already existed. Still to add: `golangci-lint`, `gosec` and the weekly `E2E_NETWORK` job. |
| 11 | Fixed | Search rows, the tool header and the sidebar score show "checked N ago", plus a `stale` tag after 14 days (two missed weekly re-checks). |
| 14 | Fixed | `httpx` logs Debug for each request (host, path, status, ms, attempt; no query strings) and Warn on rate limits. `queue` logs Info for retries and deferrals and Warn for dead jobs. The Docker sandbox logs Info for install, start and stop (image, runtime, exit code, ms). The runner logs one `stage done` line per tool (stage, tool, job, decision, ms, notes) and the stage error once. |
| 15 | Fixed | `config.Validate` refuses to start when `SITE_URL` is https and `DATABASE_URL` is the development default. Covered by a unit test. |
| 16 | Fixed | New `release.yml`: a `vX.Y.Z` tag pushes the images to GHCR, tagged with the version and `sha-…`. Set `MCPKG_IMAGE` in `.env` and `bootstrap.sh` pulls instead of building. Docker Engine is pinned (`DOCKER_VERSION=28.5`, falls back to the latest). |
| 18 | Fixed | CSP `img-src` allows only GitHub raw/user-content, shields.io, badge.fury.io, badgen and codecov. |
| 19 | Not an issue | `ADMIN_PASS` and `PG_PASS` are shell variables in `make-env.sh`. They are not written to `.env`. |
| 21 | Fixed | Removed `registry.npmjs.org` from the squid ACL (already covered by `.npmjs.org`). |
| 22 | Fixed | Added `crates.io`, `index.crates.io` and `static.crates.io`. The smoke stage still skips Rust crates (planned for Phase 3). |
| 23 | Fixed | Bumped to `checkout@v6`, `setup-go@v6`, `setup-buildx-action@v4` and `setup-qemu-action@v4`. |

Sections 3–6 are feature work and are not in this round: GitHub sign-in and claims, the metadata stage and npm-style tabs, benchmarks.

## 10. Verification of the fix round (2026-11-07, review)

CI on `main` (merge `a17624b`, run 42387690960): the `go` job and the `docker` job both pass. All three images build and `squid -k parse` accepts the new ACL. Each row below was checked against the files on `main`, not against the commit message.

| # | Claimed | Verified | Notes |
|---|---|---|---|
| 1 | Open | Open | Unchanged by design; depends on section 3. |
| 2 | Fixed | Fixed | `--platform=$BUILDPLATFORM` plus `GOARCH=$TARGETARCH`; CI builds amd64 and arm64. |
| 3 | Fixed | Fixed | `LOG_DIR=-` in the image, the compose anchor, `make-env.sh` and `.env.example`. |
| 4 | Fixed | Fixed | `<<: *env` merged in `worker-light` and `worker-heavy`. |
| 5 | Fixed | Fixed | `USER 10001`; only `worker-heavy` is `user: root`. |
| 6 | Fixed | Fixed | `@https protocol https` matcher; `DOMAIN` default `:80`. |
| 7 | Already done | OK | |
| 8 | Fixed | Fixed | `on_error: "stop"`. |
| 9 | Fixed | Fixed | 300/h global cap, 8 KiB body, 500-char URL, last XFF hop. See 26. |
| 10 | Partly | Partly | golangci-lint, gosec and the weekly `E2E_NETWORK` job are still missing. |
| 11 | Fixed | Not inspected | Template-only change; check in the browser after deploy. |
| 14 | Fixed | **Partly** | 10 of 44 non-test files log. Boundaries are covered (httpx, queue, docker sandbox, runner). Still silent: `llm/llm.go` (calls, tokens, cost), `scan/scan.go` and `scan/extract.go` (scanner exit codes), `mcpclient/*` (smoke handshake), `submit/submit.go`, `store/store.go` (query errors), `sandbox/process.go`, `sources/*`. Do llm, scan, mcpclient and submit first. |
| 15 | Fixed | Fixed | `Validate()` runs in `main.go` before the logger; unit test present. |
| 16 | Fixed | Fixed | `release.yml` on `v*` tags pushes to GHCR; bootstrap pulls when `MCPKG_IMAGE` is set. See 25. |
| 18 | Fixed | Fixed (round 3) | See 24. |
| 19 | Not an issue | Agreed | |
| 21 | Fixed | Fixed | `.npmjs.org registry.yarnpkg.com`; the proxy image builds. |
| 22 | Fixed | Fixed | `crates.io index.crates.io static.crates.io`; no overlap. |
| 23 | Fixed | Fixed | checkout@v6, setup-go@v6, buildx@v4, qemu@v4; no Node 20 warnings in run 42387690960. |

### New issues (round 3)

| # | Severity | Issue | Fix |
|---|---|---|---|
| 24 | **High** (Fixed in `36ac3e2` + `25fa772`, test in `837b12d`) | `internal/web/server.go`: the CSP header is one Go string literal that contains the text `` `+imgHosts+` `` (backticks inside double quotes are characters, not concatenation). The header sent is `img-src 'self' data: `+imgHosts+`; ...`, so the allowlist never applies and every README image and badge is blocked. `imgHosts` is unused (an unused package-level const does not fail vet). CI stayed green because nothing asserts the header. | Change to `"... img-src 'self' data: " + imgHosts + "; style-src ..."`. Add a handler test that requests `/` and asserts the CSP header contains `raw.githubusercontent.com` and no backtick. |
| 25 | Medium | `release.yml` pushes `mcpkg-runner` and `mcpkg-egress-proxy` for amd64 only. With `HEAVY_ON_CORE=1` on the arm64 core box, bootstrap pulls images that cannot run there. | Build those two for `linux/amd64,linux/arm64` as well, or make bootstrap refuse `HEAVY_ON_CORE=1` when `uname -m` is `aarch64` and `MCPKG_PROXY_IMAGE` is set. |
| 26 | Low | `clientKey` trusts the last `X-Forwarded-For` hop. Correct behind Caddy, but only while `web:8080` is not reachable from outside. | Keep `web` without a `ports:` mapping (verify) and say so in `DEPLOY.md`; or have Caddy set `X-Real-IP` and read that instead. |
| 27 | Low | The multi-arch `docker buildx build` in `ci.yml` has no `--output`, so it only checks that the build succeeds (fine) and prints a warning each run. | Add `--output type=cacheonly`, or leave as is. |

**Next**: the remaining files in 14, then 10, then 25. Issue 24 is fixed and covered by `TestSecurityHeaders`; CI run 42395953523 on `25fa772` is green (go + docker). Note for Go edits: gofmt drops the spaces around `+` inside a multi-argument call, so write `"a"+x+"b"` there.

## 11. Round 4: logging, CI gates, benchmark stage + gate (2026-11-08)

| # | Status | What changed |
|---|---|---|
| 14 | **Fixed** | Logging is complete across the codebase. New in this round: `llm` (per-call Debug with model/ms/tokens/finish_reason, Warn on failure), `scan` (Debug summary per scan + extraction with files/bytes/ms, Warn on limits), `mcpclient` (Info per handshake with protocol/server/tool counts, Warn on failure, Debug on stdout pollution and rpc calls — via `HandshakeStdioLog`/`HandshakeHTTPLog`), `submit` (Info per submission, Warn on reject/failure), `store` (Warn on query failures at each boundary: save_stage, check_runs, search, stats, submissions, benchmarks), `sandbox/process` (Info install/start/cleanup with job, kind, bin, pid, ms), all five `sources` crawlers (Info crawl start/done with emitted/cursor/ms, Warn on per-package fetch failures). All wired from `app.New` with per-component loggers. |
| 10 | **Fixed** | `.golangci.yml` (v2) with errcheck, govet, ineffassign, misspell, staticcheck, unconvert, unused, copyloopvar; `golangci-lint-action@v9` pinned to `v2.15.0`. gosec (`securego/gosec@v2.30.0`, high severity+confidence) scans `internal/sandbox`, `internal/launch`, `internal/web` — 0 findings. New `e2e-network` job: weekly cron + `workflow_dispatch`, runs `TestFullPipeline` with real npm/PyPI installs (installs `uv` via pip). |
| 25 | **Fixed** | `release.yml` builds `mcpkg-runner` and `mcpkg-egress-proxy` for `linux/amd64,linux/arm64` too, so `HEAVY_ON_CORE=1` works on the arm64 core box. |
| — | New | **Benchmark stage** (HANDOFF next-task 2, gate spec in `docs/BENCH_GATE.md`): `internal/bench` with scanners `scorecard`, `osv`, `mcp-conformance` (SSRF-guarded), and CLI adapters (`gitleaks`, `trivy`, `semgrep`, `mcplint` — honest `skip=scanner-unavailable` until `BENCH_REPO_DIR` is provided). Every row carries scanner, version, image digest, `ran_at`, duration, exit code, verdict, raw output (≤256 KiB), and skip rows carry an enumerated reason — never a silent gap. The stage runs off the default chain (`mcpkg bench` enqueues, `mcpkg worker -stages bench` drains) behind `BENCH_ENABLED`, appends to `tool_benchmarks` (re-running replaces, never duplicates), and structurally cannot touch score/status/visibility/install. Benchmarks tab + `/api/bench/{slug}` serve the raw rows with provenance. |
| — | New | **Tier-1 deploy gate**: CI job `bench-gate` runs B0 (schema/provenance), B1 (no silent gaps), B2 (12-tool known-answer fixture, exact verdicts), B3 (determinism) and B5 (stage safety invariants) over fake upstreams, writes `bench/gate.json`, uploads it as an artifact and commits it to `main` (commit titles `gate:`; the job skips itself on those). `deploy/aws/push.sh` refuses to run without `GATE_SHA=<sha>` naming a commit whose report is green. |

**Still open (T2 soak + feature work):** B6 (perf p95 targets, freeze after baseline), B7 (cost envelope), B8 (teardown/restore drill) — all measured on a prod-shaped stack before public launch; GitHub sign-in and claims (section 3); metadata stage and npm-style tabs (section 5); Code tab and Dependents (section 6); the Bench tab's UI pass against Mobbin references.
