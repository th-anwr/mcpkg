# Plan

**Goal:** become the trust layer for MCP servers. One searchable place where every open-source server is listed and tested (health, security, smoke test), with exact install steps that people and agents can use. The official MCP Registry stays the source of truth: it explicitly invites aggregators that add ratings and security scans. We consume it hourly and add what it doesn't do.

## Positioning

| | Official registry | Big directories (Glama, Smithery, PulseMCP…) | **mcpkg** |
|---|---|---|---|
| Coverage | self-published only | large, many dead entries | registry + npm + PyPI + crates + GitHub, deduped |
| Verification | namespace ownership | some grades and badges, mostly unverified | **every package installed and handshaked in a sandbox** |
| Dead tools | stay listed | stay listed (low grade) | marked, de-ranked, hidden when malicious |
| Agent install | no | partial | `install.md`, `manifest.json`, finder MCP server, registry-compatible API |

Headline metric: **"N servers verified to start"**, a number nobody else publishes.

## Phases

### Phase 0: build (done: this repo)

Crawlers, dedupe, queue, five hooks, sandbox, scoring, install generator, site, APIs, finder, deploy kit, and tests. The end-to-end test runs real npm and PyPI installs.

### Phase 1: launch the catalog (week 1)

| Day | Step | Command |
|---|---|---|
| 1 | `.env`, launch + start the core box, budget alert ([DEPLOY.md](DEPLOY.md)) | `make-env.sh`, `launch.sh core`, `push.sh core`, `budget.sh` |
| 1 | Discovery from all sources (~1–2 h; PyPI index scan is the slowest) | automatic (scheduler) |
| 1–2 | Health on everything: GitHub GraphQL, 25 repos per call; READMEs fetched | automatic (light worker) |
| 2–6 | Security + smoke on 2–3 **spot** workers, most-popular first | `launch.sh worker` + `push.sh worker <ip>` ×2–3 |
| 2 | Site is useful already: pages show health data, "checks in progress" for the rest | |

### Phase 2: quality (week 2)

- `mcpkg eval-llm` on 2–3 open models, then run enrich (see [LLM.md](LLM.md)).
- Work the review queue. Tune static rules that cause false positives (target: <10% of reviewed findings are wrong).
- Spot-check 50 random "broken" results. Fix launcher edge cases (args, entry points).
- Add the nightly backup cron (`scripts/backup.sh`, S3).

### Phase 3: growth (weeks 3–6)

- **Distribution:**
  - README badges (`/badge/<slug>.svg`). Tell the top 200 authors their grade.
  - Show HN, r/mcp, MCP Discord.
  - Offer `/v0.1/servers` to MCP client developers.
  - Submit to awesome-mcp lists.
- **Agents:** publish the finder MCP server (`mcpkg finder`) to the official registry, so any agent can search and install.
- **Search:** semantic search (pgvector + a small open embedding model).
- **Authors:**
  - claim and verify ownership (GitHub OAuth or `/.well-known/mcpkg.json`)
  - "re-check now" button
  - appeal a finding
- **Coverage:**
  - OCI images and Rust crates in the smoke test
  - local-HTTP packages
  - remote OAuth detection
- **Depth:** functional tests (real tool calls) for the top ~100 servers, with test accounts.

### Phase 4: sustainability (month 2+)

- **Private catalogs for companies:** an approved subset with policy hooks (e.g. "no shell capability"). This is the real business: companies need allowlists of trusted MCP servers.
- **Sponsored "verified publisher" checks** and maintainer alerts (your package broke, a CVE appeared).

## Time and cost for the first full pass

Assumptions: ~50k unique servers after dedupe. About half are remote endpoints (seconds each) or source-only repos (skipped). That leaves ~25–30k packages to install and handshake.

| Stage | Per tool | Throughput | First pass |
|---|---|---|---|
| health | API only | ~25 repos per GraphQL call | hours |
| security | 20–60 s (download + scan) | I/O-bound, runs beside smoke | adds ~20–30% box time |
| smoke | 1–3 min (install dominates) | ~3 at a time per 4-vCPU box ≈ 2,000/day | **~12–15 box-days** |
| enrich | ~2k tokens | API-bound | hours (hosted) / ~1 day (one T4) |

- **One box:** ~2 weeks.
- **3 spot workers:** ~4–6 days.
- **Re-checks after that** are cheap: unchanged tools skip the heavy stages, and only new versions get re-smoked.

## Budget: $114 of AWS credits

Prices are approximate us-east-1 list or typical spot prices. Check the Spot price history in your region before launching.

| Item | Size | Month 1 | Month 2+ |
|---|---|---|---|
| Core box (Postgres, web, scheduler, light worker) | t4g.small on-demand (~$0.017/h) + 30 GB gp3 + public IPv4 (~$0.005/h) | ~$18 | ~$18 |
| First pass: security + smoke | 2–3 × c7a.xlarge spot (~$0.07–0.12/h), ~300–350 box-hours | ~$30–45 | — |
| Re-checks of changed tools | spot worker a few hours per week | — | ~$5–10 |
| LLM descriptions | hosted open model (~$3–10, outside AWS) **or** Bedrock / GPU spot on credits (~$5–15) | ~$0–15 | <$2 |
| Backups | S3, a few hundred MB | <$1 | <$1 |
| **Total** | | **~$50–80** | **~$25–30** |

**Result:** the credits cover launch plus roughly 2–3 months.

**Watch out:** on the AWS Free plan, the account closes when credits run out or after 6 months. Keep backups. Before month 3, decide between:

- upgrading the AWS account to a paid plan (~$25–30/month at this size), or
- moving to a cheaper VM host (e.g. a Hetzner ARM cloud VM for the core plus hourly cloud VMs for passes).

Moving is `pg_dump`, `bootstrap.sh`, restore.

Ways to spend less:

- Start with `-limit`ed discovery plus priority ordering, so the top 10k are verified on day 1.
- Shut the spot workers down between passes. The queue waits.
- Run the heavy profile on the core box (`HEAVY_ON_CORE=1`) only if it has ≥4 GB RAM. Too slow for the first pass, fine for re-checks.

## Success metrics

| Metric | Target after 60 days |
|---|---|
| Servers verified to start | 10k+ |
| Catalog checked in the last 14 days | > 90% |
| LLM first-try acceptance | > 90% |
| Wrong high-severity findings (from review) | < 10% |
| Repos showing the badge | 200+ |
| Agent API + finder calls | growing week over week |

## Risks

| Risk | Mitigation |
|---|---|
| Registry or a big directory ships the same checks | Be the data layer others consume: open API, subregistry, badges. Sandboxed smoke tests are costly to copy well. |
| Sandbox escape on a worker | gVisor; workers are disposable spot boxes; a separate DB role for workers with minimal grants (TODO); no other secrets on workers |
| False positives hurt authors | Findings are labelled leads; review before any cap beyond 60; appeal link; only OSV `MAL-*` or registry takedown hides |
| GitHub / registry rate limits | Token + GraphQL batching; registry polled hourly as requested; backoff without burning attempts |
| Name/domain | "mcp.net" is very likely taken. Check `mcpkg.dev`, `.net`, `.io` or another short name at a registrar before launch. Renaming is env vars + module path. |
