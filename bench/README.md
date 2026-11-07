# bench/

The tier-1 deploy gate report lives here as `bench/gate.json`:

- The `bench-gate` CI job runs `go test ./internal/bench -run TestGate` on every
  commit to `main` (B0–B3, B5 over a 12-tool known-answer fixture) and commits
  the resulting report back to `main` (commit titles start with `gate:`).
- `deploy/aws/push.sh` refuses to deploy unless `GATE_SHA=<sha>` names a commit
  whose report is green. Deploying an untested tree is a hard failure, not a
  warning.

Full gate definition: `docs/BENCH_GATE.md`. The report is data, not prose —
regenerate it with the test, never edit it by hand.
