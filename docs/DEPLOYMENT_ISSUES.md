# Deployment issues

Problems found while deploying mcpkg to AWS, with the output that shows each one. Code-review findings stay in `docs/ISSUES-NEW.md`; the issue numbers (N15 to N24) are the same in both files.

No secrets, account IDs or private keys belong in this file: the repository is public.

## Deployments so far

| Date (UTC) | Release | Boxes | Run from | Result |
|---|---|---|---|---|
| 2026-11-11 09:42 | `v0.1.0` (`a6dab94`) | core t4g.small on-demand, 2 × c7a.xlarge spot workers | Windows 11, Git Bash | Up. Needed four manual workarounds (N15 to N18). |
| 2026-11-11 10:44 | `v0.1.1` (`d0a207c`) | core only | Windows 11, Git Bash | Up on the first try with no workarounds. Database container untouched. |
| 2026-11-11 11:45 | `v0.1.2` (`fe01f26`, PR #2) | core and the remaining spot worker, in parallel | Windows 11, Git Bash | Up. Fixes N22, N20 and N24, all verified in production (see below). 328 wrongly failed tools re-queued. |
| 2026-11-11 12:32 | `v0.1.3` (`19fd7b8`) | core and the worker, in parallel | Windows 11, Git Bash | Up. Fixes N25, verified in production (see below). 355 `install_failed` tools queued for a new smoke test. |

Site: `http://23.23.102.237` (plain HTTP, no domain yet). Checks after each deploy:

```
$ curl -fsS http://23.23.102.237/healthz
ok
```

## Status

| # | Severity | Issue | Status |
|---|---|---|---|
| N22 | **High** | Smoke installs fail for every npm and PyPI server under gVisor | Fixed in `v0.1.2`, verified in production |
| N20 | Medium | npm downloads API rate limits (HTTP 429) | Fixed in `v0.1.2`: 429s down from 293–465 to 62 per 10 minutes |
| N23 | Medium | Spot worker reclaimed 38 minutes after launch | **Open**: one worker left |
| N24 | Low | `status.sh` counts install failures as "broken" | Fixed in `v0.1.2` |
| N25 | Medium | Sandbox Python 3.11 fails packages that need Python 3.12 or newer | Fixed in `v0.1.3`, verified in production |
| N26 | Medium | Proxy blocks GitHub release downloads, so npm packages with prebuilt native modules fail to install | **Open** |
| N27 | Low | Smoke test picks a CLI entry point that prints help and exits, recorded as "broken" | **Open** |
| N21 | Medium | Deploy gate skipped its checkout checks on Windows | Fixed in `879312b` |
| N15 | Medium | Git Bash rewrote AWS parameter names | Fixed in `879312b` |
| N16 | Medium | SSH key saved with Windows line endings | Fixed in `8aba0db` |
| N17 | Medium | Scripts checked out with Windows line endings | Fixed in `8aba0db` |
| N18 | Low | `python3` is the Microsoft Store alias on Windows | Fixed in `8aba0db` |
| N19 | Low | Parallel pushes race on `~/.ssh/known_hosts` | Fixed in `8aba0db` |

## Open issues

### N22 (High): smoke installs fail for every npm and PyPI server under gVisor

**Symptom.** No npm or PyPI server has passed a smoke test. Only remote servers, which need no install, pass. Each failure is written to the catalog as `install_failed` and shows on the tool's public page.

```
 smoke_status   | method | count
----------------+--------+-------
 install_failed | npm    |    97
 install_failed | pypi   |    97
 skipped        | none   |    35
 auth_required  | remote |    14
 works          | remote |    10
 broken         | remote |     3
 broken         | oci    |     1
```

Every npm failure has the same cause:

```
install failed: exit status 1 | //registry.npmjs.org/exa-mcp-server failed, reason: getaddrinfo EAI_AGAIN egress-proxy
install failed: exit status 1 | /registry.npmjs.org/@hostinger%2fmcp failed, reason: getaddrinfo EAI_AGAIN egress-proxy
```

**Cause.** Install containers run on the internal `mcpkg-egress` network with `HTTPS_PROXY=http://egress-proxy:3128` (`internal/sandbox/docker.go` line 97). Docker resolves `egress-proxy` with its embedded DNS server, and that server does not answer inside gVisor (`runsc`). The same lookup on a worker, with the runner image:

```
SANDBOX_RUNTIME=runsc
SANDBOX_EGRESS_NETWORK=mcpkg-egress
SANDBOX_PROXY=http://egress-proxy:3128
--- proxy container networks: mcpkg-egress=172.18.0.2 mcpkg_default=172.19.0.3
runc   lookup egress-proxy -> 172.18.0.2      egress-proxy
runsc  lookup egress-proxy -> LOOKUP-FAILED
```

**Fix.** Give the `egress` network a fixed subnet and `egress-proxy` a fixed `ipv4_address` in `deploy/docker-compose.yml`. Have `DockerRunner.InstallArgs` add `--add-host egress-proxy:<that address>`, since gVisor does read `/etc/hosts`. Or set `SANDBOX_PROXY` to the IP literal. Then re-queue smoke for every tool with `smoke_status = 'install_failed'`.

Do not work around it with `SANDBOX_RUNTIME=` (plain `runc`). Workers hold the database password in `.env`, so a package that escapes the container could rewrite the catalog.

**How to verify the fix.** On a worker, the `runsc` lookup above returns the proxy address. Within an hour of redeploying, `works` grows for the `npm` and `pypi` methods.

**What was changed (PR #2).** A different route from the one proposed above, with no compose, network or `.env` change on the boxes. `DockerRunner` asks Docker which container on the egress network answers to the proxy name, and gives the sandbox `HTTPS_PROXY`/`HTTP_PROXY` with that address, so nothing inside the sandbox needs DNS. The address is cached for a minute and looked up again after any failed install. Setting `SANDBOX_PROXY` to an address still works and skips the lookup.

Two cases are now retried as infrastructure errors and no longer written to the tool's page as `install_failed`: the proxy cannot be found while the runtime is gVisor, or the install output shows the proxy itself was unreachable.

**Test.** The new CI job `sandbox-e2e` installs gVisor the way `bootstrap.sh` does, starts the proxy image on an internal network next to the default one, and installs `semver` (npm) and `wheel` (PyPI) in the sandbox. Run 43011725542, Docker 28.0.4, runsc release-20261102.0:

```
runc    by name:     install works
runc    by address:  installed semver@7.6.3, wheel==0.45.1
runsc   by name:     npm error request to https://registry.npmjs.org/semver failed, reason: getaddrinfo EAI_AGAIN egress-proxy
runsc   by address:  installed semver@7.6.3, wheel==0.45.1
```

The `runsc by name` line is the production failure, reproduced. This was not tested on the AWS worker itself.

**After deploying to the workers,** re-queue the tools that were marked wrongly (about 194 at the time of writing):

```
./scripts/mcpkg.sh enqueue -stage smoke -smoke-status install_failed
```

### N20 (Medium): npm downloads API rate limits

**Symptom.** The light worker on the core box logs a steady stream of:

```
level=WARN msg="rate limited" component=http host=api.npmjs.org status=429 until=2026-11-11T10:00:22Z
```

**What `v0.1.1` changed.** The health stage asks npm for up to 128 unscoped packages per request instead of one request per tool. Measured on the core box:

| 10-minute window | npm 429 warnings | Health checks completed |
|---|---|---|
| Before `v0.1.1` | 293 | about 4,550 |
| After `v0.1.1` | 465 | about 3,575 |

Share of checked npm tools that got a download count:

```
 utc_10min | npm_tools_checked | with_downloads | pct
-----------+-------------------+----------------+------
 10:00     |               419 |            395 | 94.3
 10:10     |               428 |            394 | 92.1
 10:20     |               421 |            384 | 91.2
 10:30     |               367 |            351 | 95.6
 10:40     |               431 |            392 | 91.0    <- v0.1.1 deployed at 10:44
 10:50     |               408 |            345 | 84.6
```

**Cause.** Most npm MCP packages are scoped, and npm's bulk endpoint does not accept scoped names:

```
 npm_tools | scoped | pct_scoped
-----------+--------+------------
     22339 |  12794 |       57.3
```

So more than half the lookups are still one request each. They are now sent back to back at the start of each batch of 25 tools, which is burstier than before. The worst second saw three 429 responses.

**Fix.** `httpx.New` already keeps a minimum gap between requests for `crates.io`, `pypi.org` and `api.github.com`, but has none for `api.npmjs.org`. Add one, for example one request per second, and keep the bulk path for unscoped names. Measure again with the coverage query in `docs/ISSUES-NEW.md` section 7.

**What was changed (PR #2).** `httpx.New` paces `api.npmjs.org` at one request per second. Demand is about 0.4 scoped lookups per second, so this spreads the bursts without slowing the stage. `NPM_API_GAP_MS` in `.env` overrides the gap without a release. npm does not publish the limit, so one per second is a first guess: after deploying to the core box, count the 429 warnings and run the coverage query again.

### N23 (Medium): spot worker reclaimed

```
LaunchTime              StateTransitionReason                          StateReason
2026-11-11T09:55:16Z    Service initiated (2026-11-11 10:33:45 GMT)    Server.SpotInstanceTermination
```

AWS reclaimed the worker 38 minutes after launch. Both workers had been placed in us-east-1a, which was not the cheapest zone at launch time:

```
us-east-1f   0.078700
us-east-1c   0.080500
us-east-1a   0.090500
```

Nothing was lost: the database is on the core box, and the worker's jobs go back to the queue when their leases expire. `~/.mcpkg/workers.list` on the deploying machine still lists the dead worker.

**Fix.** Let `launch.sh` pick the cheapest zone, or fall back across c7a, c6a and m7a. Drop dead entries from `workers.list`.

### N24 (Low): install failures counted as "broken"

`status.sh` printed `smoke: 10 works, 14 need key/auth, 198 broken`, which read as 198 broken servers. 194 of them were the sandbox install failures from N22. Report `install_failed` as its own count.

**What was changed (PR #2).** `mcpkg stats`, which `status.sh` prints, now reads `smoke: 10 works, 14 need key/auth, 4 broken, 194 install failed`. The home page still shows the two together as "fail to start".

## Fixed issues

All five were tested on Windows 11 with Git Bash against the real AWS account, then by the `v0.1.1` deploy, which needed no workarounds.

### N15: Git Bash rewrote AWS parameter names

```
$ aws ssm get-parameter --name /aws/service/canonical/ubuntu/server/24.04/stable/current/arm64/hvm/ebs-gp3/ami-id ...
aws: [ERROR]: An error occurred (ParameterNotFound) when calling the GetParameter operation
```

Git Bash turns arguments that start with `/` into Windows paths before they reach the native `aws.exe`. Fixed in `879312b`: `aws_` in `deploy/aws/lib.sh` sets `MSYS_NO_PATHCONV=1` for each AWS call only.

### N21: deploy gate skipped its checkout checks on Windows

The first fix for N15 exported `MSYS_NO_PATHCONV=1` for the whole script. That also stopped the `/c/...` path translation that the native `git.exe` needs, so the gate's "checkout is exactly `GATE_SHA` and clean" check silently returned early:

```
+ require_lf_scripts
+ command -v git
+ git -C /c/Users/.../crlf-old rev-parse --git-dir
+ return 0
```

The first deploy ran with the variable exported by hand, so those checks did not run then. The checkout was in fact clean at `v0.1.0`. After the fix in `879312b`, the gate refuses both cases on Windows:

```
xx this checkout is not at v0.1.0: run 'git checkout v0.1.0' first
xx uncommitted changes in the checkout: commit and push them so CI tests what you deploy
```

### N16: SSH key saved with Windows line endings

```
Load key "/c/Users/anwr/.ssh/mcpkg.pem": invalid format
ubuntu@23.23.102.237: Permission denied (publickey).
CR bytes: 8
```

The Windows AWS CLI prints the new key with CRLF line endings. Fixed in `8aba0db`: `ensure_key` strips `\r`. Tested by creating and then deleting a real key pair: 0 carriage returns, and `ssh-keygen -y` parses it.

### N17: scripts checked out with Windows line endings

Git for Windows defaults to `core.autocrlf=true` (`C:/Program Files/Git/etc/gitconfig`), and the repository had no `.gitattributes`. A normal clone gets CRLF scripts, and `push.sh` uploads the working tree as it is:

```
CR bytes in old clone push.sh: 67
```

Fixed in `8aba0db`: `.gitattributes` (`* text=auto eol=lf`), plus `require_lf_scripts`, which refuses an older CRLF clone:

```
xx Windows line endings in: .github/workflows/ci.yml .github/workflows/release.yml .golangci.yml deploy/Caddyfile deploy/aws/allow-my-ip.sh ...
```

A new clone with `core.autocrlf=true` now has 0 carriage returns in every script.

### N18: `python3` is the Microsoft Store alias

```
Python was not found; run without arguments to install from the Microsoft Store, or disable this shortcut from Settings > Apps > Advanced app settings > App execution aliases.
```

`require_green_ci` needs Python to read GitHub's check runs. Fixed in `8aba0db`: it falls back to `python` when `python3` does not run.

```
deploy gate green for v0.1.0: go, docker, bench-gate
```

### N19: parallel pushes race on `~/.ssh/known_hosts`

```
update_known_hosts: hostfile_replace_entries failed for /c/Users/anwr/.ssh/known_hosts: Permission denied
```

Harmless, but it appeared when two workers were pushed at once. Fixed in `8aba0db` with `UpdateHostKeys=no`. Five rounds of four parallel connections, each starting from an empty known-hosts file:

```
old: 6 known_hosts warnings in 5 rounds of 4 parallel connections
new: 0 known_hosts warnings in 5 rounds of 4 parallel connections
```

## `v0.1.2` deploy and production checks (2026-11-11 11:45 UTC)

CI on `fe01f26` was green for `go`, `docker`, `bench-gate` and the new `sandbox-e2e` job. The `release` workflow published all three images for amd64 and arm64. The core box and the worker were deployed in parallel with `GATE_SHA=v0.1.2 ./deploy/aws/push.sh`, and the database container was not restarted.

### N22: sandbox installs under gVisor

The install path, tested on the worker with the `v0.1.2` runner image under `runsc` on the egress network, with the proxy given by address as the new code does:

```
--- gVisor + proxy by address: npm
7.0.0                                   <- npm view is-number version
--- gVisor + proxy by address: PyPI
pypi http 200
--- host not on allowlist, through proxy (expect refusal)
example.com refused: <urlopen error Tunnel connection failed: 403 Forbidden>
--- direct, no proxy (expect no route)
direct refused: <urlopen error [Errno -3] Temporary failure in name resolution>
```

Smoke results in the first 20 minutes after the deploy. Before it, npm and PyPI had no `works` at all:

```
 method |   smoke_status    | count
--------+-------------------+-------
 npm    | broken            |    11
 npm    | install_failed    |     1
 npm    | needs_credentials |     3
 npm    | works             |     9
 pypi   | broken            |    13
 pypi   | install_failed    |     7
 pypi   | needs_credentials |     3
 pypi   | works             |     9
```

None of the new failures are the proxy problem. One PyPI failure with a DNS error, `iflow-mcp-asaotomo-fofamap`, was checked at 11:45:58, before the worker restarted on `v0.1.2` at 11:47:02. The others are dependency or build failures in the packages themselves, plus N25 below.

The tools that had been marked wrongly were re-queued on the core box:

```
$ ./scripts/mcpkg.sh enqueue -stage smoke -smoke-status install_failed
level=INFO msg=enqueued stage=smoke matched=328 new_jobs=328
```

They share the smoke queue (about 15,650 jobs) with everything else, so with one worker at two jobs at a time they are worked off over the following days, not hours.

### N20: npm pacing

| 10-minute window | npm 429 warnings | Health checks completed |
|---|---|---|
| Before `v0.1.1` | 293 | about 4,550 |
| After `v0.1.1` (batching only) | 465 | about 3,575 |
| After `v0.1.2` (batching plus one request per second) | 62 | about 2,450 |

Share of checked npm tools that got a download count: 95.2% to 98.6% per 10-minute window after `v0.1.2`.

To watch: health checks per 10 minutes were lower in the first window after `v0.1.2`. The cause is not measured. If it stays low, try `NPM_API_GAP_MS=500` in `.env` and compare.

### N24: install failures counted separately

```
smoke: 35 works, 50 need key/auth, 32 broken, 334 install failed; 7 blocked, 249 unmaintained
```

### N25 (Medium, open): sandbox Python is too old for some PyPI servers

The runner image has Python 3.11.2 (`uv 0.5.11`, Node `v22.23.4`). Packages that require Python 3.12 or newer fail to resolve and are recorded as `install_failed`, which reads as a broken package:

```
× No solution found when resolving dependencies:
╰─▶ Because the current Python version (3.11.2) does not satisfy Python>=3.12 and basic-memory==1.0.0
    depends on Python>=3.12, we can conclude that basic-memory==1.0.0 cannot be used.
```

6 of the 36 PyPI smoke results in the first 20 minutes after `v0.1.2` failed this way, about 1 in 6.

**Fix.** Install a newer Python into the runner image at build time (for example `uv python install 3.13 3.12`), and have the PyPI install use it: pass `--python 3.13`, or retry with a newer Python when uv reports `does not satisfy Python>=`. Then re-queue the affected tools with `enqueue -stage smoke -smoke-status install_failed`.

## `v0.1.3` deploy: N25 fixed (2026-11-11 12:32 UTC)

**Change** (`37285e2`, `19fd7b8`). The runner image ships uv-managed Python 3.12 and 3.13 next to the system Python 3.11. A PyPI install still tries 3.11 first, named explicitly, so packages that worked before install exactly as before. Only when uv reports that the package needs a newer Python does the install retry with 3.13, then 3.12. Any other failure stops at once with the original error.

**Caught in CI before release.** The first version never retried: uv wraps its message across lines, so the text did not match.

```
╰─▶ Because the current Python version (3.11.2) does not satisfy
    Python>=3.12 and django==6.0 depends on Python>=3.12, we can conclude
```

`19fd7b8` matches on whitespace-collapsed output. `sandbox-e2e` now installs `django==6.0` (requires Python 3.12 or newer) and checks that the retry happens for it and not for `wheel`:

```
installed wheel==0.45.1 under docker+runsc, bin=wheel
installed django==6.0 under docker+runsc, bin=django-admin
--- PASS: TestDockerInstallThroughProxy/pypi-needs-py312 (6.26s)
```

**Production checks on the worker.** The worker's runner image:

```
Python 3.11.2
cpython-3.13.1-linux-x86_64-gnu    (managed)
cpython-3.12.8-linux-x86_64-gnu    (managed)
cpython-3.11.2-linux-x86_64-gnu    /usr/bin/python3.11
```

The deployed install script, run the way the sandbox runs it (gVisor, egress network, proxy by address). `basic-memory` is a catalog server that had failed with the error above:

```
basic-memory==1.0.0: exit=0 in 28s, venv=Python 3.13.1, retried=1, bins=MCPKG_BINS=["basic-memory", "bm"]
wheel==0.45.1:       exit=0 in 1s,  venv=Python 3.11.2, retried=0, bins=MCPKG_BINS=["wheel"]
```

The run phase (gVisor, `--network none`, `--read-only`, all capabilities dropped) starts the server installed with 3.13:

```
$ /work/venv/bin/basic-memory --version
Basic Memory version: 1.0.0
```

**Re-queue.** Every tool marked `install_failed` now has a smoke job waiting:

```
level=INFO msg=enqueued stage=smoke matched=355 new_jobs=27
 install_failed_tools | with_smoke_job_waiting
                  355 |                    355
```

## Retries and queue order

- Failed installs are retried automatically every week. After rendering a tool, the pipeline schedules its next full check, smoke test included, seven days later (`recheck.health_every_days` in `pipeline.json`). This applies to every tool that is not blocked.
- Jobs queued by hand with `./scripts/mcpkg.sh enqueue` get priority 0, the lowest. New tools are ordered by popularity, and within a priority level older jobs go first, so re-queued failures run after everything already waiting and do not hold up the worker.

## Checks after `v0.1.3` (2026-11-11, about 13:00 UTC)

No errors or panics on either box since the deploy. The site returned no 5xx responses, and no sandbox containers or volumes were left behind. Core: disk 26% used, 1 GB of memory free, load 0.3. Worker: disk 32% used, load about 5 on 4 vCPUs, so the worker is fully busy and running more smoke jobs per worker would not help much. One security job died on `extract wheel: archive exceeds extraction limits`, which is the size cap working as intended.

Smoke results in the first 30 minutes after `v0.1.3`:

```
 npm    works 53   broken 13   needs_credentials 3   install_failed 7
 pypi   works  7   broken 33   needs_credentials 3   install_failed 3
 oci    works  3   broken  2
 remote works  4   broken  1   auth_required 8
```

### N26 (Medium, open): proxy blocks GitHub release downloads

npm packages with native modules (`better-sqlite3`, `keytar`) download a prebuilt binary from a GitHub release during install. GitHub now redirects release downloads to `release-assets.githubusercontent.com`, which is not on the allowlist, so the proxy refuses it. The install then falls back to compiling from source, which fails because the runner image has no compiler:

```
npm error path /work/app/node_modules/better-sqlite3
npm error command sh -c prebuild-install || node-gyp rebuild --release
npm error prebuild-install warn install tunneling socket could not be established, statusCode=403
```

Refused hosts in the proxy log since the deploy:

```
     13 release-assets.githubusercontent.com
      7 nodejs.org
```

`nodejs.org` is the source-build fallback fetching Node headers. It would not help without a compiler. Affected so far: 5 of 76 npm smoke results (about 7%), all marked `install_failed` although the package is fine.

**Fix.** Add `release-assets.githubusercontent.com` to the `registries` ACL in `sandbox/squid.conf`. It serves the same release files that `objects.githubusercontent.com` (already allowed) used to serve. Then release, deploy the worker, and re-queue with `enqueue -stage smoke -smoke-status install_failed`. Not needed: `nodejs.org` or a compiler in the runner image, which would widen the sandbox for the fallback path only.

### N27 (Low, open): CLI entry points that need a subcommand

Many of the 33 PyPI "broken" results are real package problems, such as a pydantic `TypeError` inside the package, or a tool that refuses to start without a `.gitignore`. Some, though, are command-line tools whose chosen entry point prints its help and exits, because the MCP server is behind a subcommand such as `serve` or `mcp`:

```
Commands: fetch  Fetch market data and save to CSV.  list-strategies ...  run  Run a backtest ...
```

These are recorded as `broken` ("server closed the connection") although the server may work. **Fix.** When the process exits after printing a command list, retry with the package's declared arguments from the registry entry, or with a common subcommand (`serve`, `mcp`, `stdio`), before deciding `broken`. Needs care to avoid running arbitrary subcommands.
