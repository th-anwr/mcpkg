# bench/

`TestGate` (`go test ./internal/bench -run TestGate`, needs `TEST_DATABASE_URL`)
writes the tier-1 gate report to `bench/gate.json`: B0–B3 and B5 over a
12-tool known-answer fixture with fake upstreams.

- In CI the `bench-gate` job runs it on every push and pull request and keeps
  the report as the `bench-gate-report` artifact. The file is gitignored.
- `deploy/aws/push.sh` deploys only commits whose `go`, `docker` and
  `bench-gate` jobs are green on GitHub (`require_green_ci` in
  `deploy/aws/lib.sh`).

Full gate definition: `docs/BENCH_GATE.md`. The report is data, not prose:
regenerate it with the test, never edit it by hand.
