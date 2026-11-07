# Deploy runbook (AWS, from CloudShell)

Everything runs from **AWS CloudShell** in the browser: no installs, no GitHub repo needed, no keys pasted anywhere. Allow about 60 minutes of hands-on time; the catalog then fills overnight.

## Before you start (10 minutes, ideally earlier in the day)

1. **Region.** In the AWS console, pick **US East (N. Virginia) us-east-1** (top right). It has the cheapest spot prices.
2. **Quotas.** Go to Service Quotas → Amazon EC2 and check two values. Request an increase if either is too low; approval can take a few hours.
   - "Running On-Demand Standard (A, C, D, H, I, M, R, T, Z) instances": **≥ 2** vCPUs
   - "All Standard (A, C, D, H, I, M, R, T, Z) Spot Instance Requests": **≥ 8** vCPUs (2 workers)
3. **Credits.** Go to Billing → Credits. Note the remaining amount and the expiry date.
4. **GitHub token.** GitHub → Settings → Developer settings → Fine-grained tokens → Generate:
   - Repository access: *Public repositories (read-only)*
   - Expiry: 90 days
   - Copy the token.
5. **Release tag.** On GitHub: Releases → Draft a new release → tag `v0.1.0` on `main` → Publish. The `release` workflow builds the three images and pushes them to GHCR (about 15 min). Then open Packages on your profile and set `mcpkg`, `mcpkg-runner` and `mcpkg-egress-proxy` to public, so the boxes can pull them without a login.
6. **Deploy gate.** `push.sh` deploys only a commit whose CI passed on GitHub (`go`, `docker` and `bench-gate`; see `docs/BENCH_GATE.md`). Check that the Actions run for the tagged commit is green. You pass the tag as `GATE_SHA=v0.1.0` below. Break-glass, for a fix you must ship before CI finishes: `GATE_OVERRIDE="<reason>"`, printed in red.

## Tonight

### 1. Open CloudShell and get the code (2 min)

Open CloudShell (the `>_` icon in the console top bar), then:

```bash
git clone https://github.com/th-anwr/mcpkg.git && cd mcpkg && git checkout v0.1.0
```

`push.sh` uploads this checkout, and the gate checks that it is exactly the tagged commit with no local edits.

### 2. Create `.env` (2 min)

```bash
./scripts/make-env.sh
```

- Press Enter to accept the defaults.
- Paste the GitHub token when asked.
- Leave the LLM empty for tonight: descriptions come from the READMEs, and an LLM can be added later.
- The script prints your `/admin/review` password. Save it.
- Use the prebuilt images, so the 2 GB core box pulls instead of building:
  ```bash
  sed -i '/^\(MCPKG_IMAGE\|MCPKG_PROXY_IMAGE\|SANDBOX_IMAGE\)=/d' .env
  cat >> .env <<'EOF'
  MCPKG_IMAGE=ghcr.io/th-anwr/mcpkg:v0.1.0
  MCPKG_PROXY_IMAGE=ghcr.io/th-anwr/mcpkg-egress-proxy:v0.1.0
  SANDBOX_IMAGE=ghcr.io/th-anwr/mcpkg-runner:v0.1.0
  EOF
  ```

### 3. Launch the core box (3 min)

```bash
./deploy/aws/launch.sh core
```

This creates:
- the `mcpkg` key pair, saved to `~/.ssh/mcpkg.pem` in CloudShell
- the `mcpkg` security group
- one **t4g.small** with an Elastic IP

It prints the IP.

### 4. Install and start it (10–15 min)

```bash
GATE_SHA=v0.1.0 ./deploy/aws/push.sh core
```

This copies the code and `.env`, installs Docker (pinned, `DOCKER_VERSION=28.5`), builds the images and starts the database, website, scheduler and light worker.

- With the image lines from step 2 the box pulls the `v0.1.0` images. Without them it builds on the box (slower, and the 2 GB core can run out of memory).
- **Postgres for workers:** `bootstrap.sh` sets `DB_BIND` to the core box's private IP. The `mcpkg` security group allows 5432 only from other `mcpkg` boxes, so the database is never public.
- **Admin password:** `/admin/*` uses HTTP Basic auth. Until a domain with HTTPS is set up, the password is sent in clear text, so avoid using it on untrusted networks.

**Checkpoint:** open `http://<core-ip>` in a browser and confirm the home page loads. Or run `curl -fsS http://<core-ip>/healthz`, which should print `ok`.

**Discovery starts by itself.** Within about 10 minutes the official registry is crawled. npm, PyPI, crates.io and GitHub take 30–90 minutes. Check with:

```bash
./deploy/aws/ssh.sh core sudo /opt/mcpkg/scripts/status.sh
```

### 5. Launch two spot workers (10–15 min, can run in parallel)

```bash
./deploy/aws/launch.sh worker          # prints the worker IP
GATE_SHA=v0.1.0 ./deploy/aws/push.sh worker <worker-ip>
# repeat for a second worker (a second CloudShell tab works in parallel)
```

Each worker is a **c7a.xlarge spot** instance. It runs security scans and sandboxed smoke tests, pulling jobs from the core's queue (most popular tools first).

**Checkpoint:** in `status.sh` output, the `security` and `smoke` rows show `running` > 0, and `smoke: N works` keeps growing.

### 6. Safety nets (3 min)

Budget alert, at 50%, 80% and 100% of $40/month:

```bash
EMAIL=<an address you read> LIMIT=40 ./deploy/aws/budget.sh
```

Nightly database backup on the core box:

```bash
./deploy/aws/ssh.sh core "echo '15 3 * * * root cd /opt/mcpkg && ./scripts/backup.sh >> /var/log/mcpkg-backup.log 2>&1' | sudo tee /etc/cron.d/mcpkg-backup"
```

That's it for tonight. Running cost: about $0.60/day for the core box and about $2–2.50/day per spot worker.

## Tomorrow morning (5 min)

```bash
./deploy/aws/ssh.sh core sudo /opt/mcpkg/scripts/status.sh
```

| What you see | What it means / do |
|---|---|
| `dead` jobs > 0 | Look at `last_error` (query below). A few are normal (packages that vanished). |
| `smoke: 0 works` after hours | Smoke tests are failing on the workers. See Troubleshooting. |
| `queued` for health barely moving | GitHub rate limit or token problem. Check the worker-light logs. |

```bash
./deploy/aws/ssh.sh core "sudo /opt/mcpkg/scripts/sql.sh \"select stage, left(last_error,120) err, count(*) from jobs where state='dead' group by 1,2 order by 3 desc limit 10\""
```

## Later

- **After the first full pass** (about 4–6 days, when `security`/`smoke` queued ≈ 0): `./deploy/aws/teardown.sh workers`. Relaunch one worker weekly for re-checks.
- **Domain:**
  1. Point an A record at the core IP.
  2. In CloudShell, set `DOMAIN=yourdomain.com` and `SITE_URL=https://yourdomain.com` in `.env`.
  3. Run `GATE_SHA=v0.1.0 ./deploy/aws/push.sh core`. Caddy gets the TLS certificate automatically.
- **LLM descriptions (Bedrock):**
  1. Enable an open-weight model in the Bedrock console and create a Bedrock API key.
  2. Set these in `.env`:
     ```
     LLM_BASE_URL=https://bedrock-mantle.us-east-1.api.aws/v1
     LLM_API_KEY=<key>
     LLM_MODEL=openai.gpt-oss-20b
     ```
  3. Run `GATE_SHA=v0.1.0 ./deploy/aws/push.sh core`.
  4. Test quality on 50 tools: `./deploy/aws/ssh.sh core sudo /opt/mcpkg/scripts/mcpkg.sh eval-llm -n 50`.
  5. Re-describe everything: `... mcpkg.sh enqueue -stage enrich -all`.
- **Code changes:** merge to `main`, tag a new release (`v0.1.1`), wait for green CI, then in CloudShell `git fetch --tags && git checkout v0.1.1`, update the image tags in `.env`, and run `GATE_SHA=v0.1.1 ./deploy/aws/push.sh core` (and each worker). Pushing again is safe.

## Troubleshooting

| Problem | Fix |
|---|---|
| SSH or `push.sh` hangs at "waiting for SSH" | CloudShell's IP changed: `./deploy/aws/allow-my-ip.sh` |
| `launch.sh worker`: `MaxSpotInstanceCountExceeded` / quota errors | Request the spot vCPU quota increase, or use on-demand: `SPOT=0 ./deploy/aws/launch.sh worker` |
| `InsufficientInstanceCapacity` (spot) | Try another type: `WORKER_TYPE=c6a.xlarge` (or `m7a.xlarge`) `./deploy/aws/launch.sh worker` |
| Spot not allowed on a Free-plan account | `SPOT=0`, or upgrade the account to the paid plan (check that credits carry over in Billing) |
| Site down | `./deploy/aws/ssh.sh core sudo /opt/mcpkg/scripts/compose.sh logs --tail 50 web caddy` |
| All smoke jobs fail with "sandbox infrastructure error" | Check gVisor on the worker: `./deploy/aws/ssh.sh <worker-ip> sudo docker run --rm --runtime runsc hello-world`. If that fails, set `SANDBOX_RUNTIME=` (empty) in `.env` and re-push the worker. |
| Core box out of memory during the build | It has 2 GB of swap; if it still fails, relaunch with `CORE_TYPE=t4g.medium`. |
| Start over | `./deploy/aws/teardown.sh all` (deletes the database: back up first) |

## What the scripts create

| Resource | Name / tag | Removed by |
|---|---|---|
| Key pair | `mcpkg` (private key in CloudShell `~/.ssh/mcpkg.pem`) | manual |
| Security group | `mcpkg`: 80/443 open, 22 from your CloudShell IP, 5432 only between mcpkg boxes | manual |
| Core instance + Elastic IP | `mcpkg-core` | `teardown.sh all` |
| Spot workers | `mcpkg-worker` | `teardown.sh workers` |
| Budget | `mcpkg-monthly` | manual |
