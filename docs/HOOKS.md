# Pipeline hooks

The pipeline is a chain of hooks configured in `pipeline.json`:

```
health → security → smoke → enrich → render
```

| Field | Meaning |
|---|---|
| `enabled` | disabled stages are skipped over; the chain continues to the next one |
| `concurrency` | parallel loops per worker process |
| `batch` | jobs claimed per loop (health uses 25: one GitHub GraphQL call for 25 repos) |
| `timeout_sec` | per-batch timeout; the queue lease is set longer automatically |
| `max_attempts` | retries before a job is parked as `dead` |
| `next` | the stage enqueued on `continue` |

Gates (`pipeline.json → gates`):

| Gate | Default | Effect |
|---|---|---|
| `skip_heavy_if_archived` | true | archived/disabled repos skip security + smoke + enrich |
| `skip_heavy_if_no_commit_days` | 0 (off) | treat long-inactive repos like archived ones |
| `stop_on_malicious` | true | OSV `MAL-*` / registry takedown → hidden |
| `review_on_high_findings` | true | high static findings open a review item |
| `skip_unchanged_within_days` | 30 | weekly re-checks skip heavy stages when version + commit are unchanged |

## Writing a script hook

Any executable works. It receives one JSON object on stdin and prints one on stdout:

```json
// stdin
{"stage": "health", "decision": "continue", "tool": { ...full tool record... }, "result": { ...that stage's result... }}
// stdout
{"decision": "continue", "notes": ["shown in the check history"], "review": "", "data": {"anything": "stored"}}
```

- `decision` can be `continue`, `skip_heavy` or `stop`. Hooks can only make the pipeline stricter: the strictest decision wins.
- `review` (optional) opens a human review item with that reason.
- On a non-zero exit, invalid JSON or a timeout, the hook follows `on_error`: `continue` (default) or `stop`.

Register it:

```json
"hooks": [
  {"name": "require-license", "after": "health", "command": ["python3", "hooks/examples/require_license.py"], "timeout_sec": 10, "enabled": true}
]
```

Ideas:

- **deny-list**: stop known-bad publishers or package names.
- **license policy**: send GPL or missing-license tools to review.
- **org policy**: private deployments can stop anything that requests the `shell` capability.
- **notify**: post newly verified A-grade tools to a channel. Return `continue` and do the side effect.

## Writing a built-in stage

Implement `pipeline.Stage`:

```go
type Stage interface {
    Name() string
    Run(ctx context.Context, tools []*model.Tool) []pipeline.Result // one result per tool
}
```

`Result.Fields` updates whitelisted tool columns (see `store.updatable`). `Result.Err` retries the job with backoff. Register it in `internal/app/app.go` and add it to `pipeline.json`.

## Running one tool by hand

```
mcpkg run -slug io.github.modelcontextprotocol.time
mcpkg run -slug io.github.modelcontextprotocol.time -from smoke
```

This prints each stage's decision, notes and result, and saves them like a worker would.
