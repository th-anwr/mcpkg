# Trust score (formula 2026.10-1)

The score is computed by `internal/score` from stored check results. The same inputs always give the same score. No LLM is involved. Every page shows the parts, the reasons and any caps.

```
total = 0.30·maintenance + 0.30·security + 0.25·works + 0.15·community   (then caps)
grade: A ≥ 85 · B ≥ 70 · C ≥ 55 · D ≥ 40 · F < 40
```

## Maintenance (30)

`0.70·activity + 0.15·release + 0.15·pull-request hygiene`

- **Activity** (last commit, or last publish if there is no repo): ≤30 d → 100, ≤90 d → 85, ≤180 d → 65, ≤365 d → 40, older → 15, unknown → 30.
- **Release**: GitHub release < 180 d → 100, older → 60. Otherwise npm/PyPI publish < 180 d → 90, older → 55. None → 30.
- **PR hygiene**: `(merged + closed/2) / (merged + closed + open)`. No PRs → 70. Oldest open PR > 6 months → −20.
- Archived or disabled repo → at most 10.

## Security (30)

Start at 100 and subtract:

- Static findings: high −25, medium −8, low −2.
- Dependency vulnerabilities: critical −30, high −15, medium −3 (capped at −50).
- Each suspicious tool description (tool poisoning, hidden Unicode): −40.
- Then blend with OpenSSF Scorecard when available: `0.75·ours + 0.25·(scorecard×10)`.
- Not scanned (no artifact) → 50. Blocked → 0.

## Works (25)

| Smoke result | Points |
|---|---|
| works | 100 (−30 if it exposes nothing, −10 for logs on stdout, −10 for startup > 20 s) |
| auth_required | 75 (hosted servers that need a login are normal) |
| needs_credentials | 70 (exits without its API key; that's expected, not a failure) |
| skipped | 40 (source-only, Rust build, local HTTP server) |
| broken | 10 |
| install_failed | 0 |

## Community (15)

`0.7·max(stars, downloads) + 0.3·contributors` (+10 for a verified GitHub org or an MCP Registry publisher)

- Stars: `100·log10(stars+1)/log10(5000)` (5,000★ = 100).
- Downloads: `100·log10(weekly+1)/log10(100000)`.
- Contributors: ≥10 → 100, ≥3 → 70, 1–2 → 40, unknown → 30.

## Caps (applied last)

| Condition | Max total |
|---|---|
| Blocked (OSV malicious or registry takedown) | 0 (hidden) |
| Suspicious tool descriptions | 40 |
| Archived repository | 40 |
| Deprecated in the MCP Registry | 30 |
| Fails to install or start | 50 |
| High-severity static findings awaiting review | 60 |

## Changing the formula

Edit `internal/score/score.go`, bump `Version`, run `go test ./internal/score`, then re-render everything without re-running checks:

```
mcpkg enqueue -stage render -all
```
