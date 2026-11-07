-- Benchmarks: one row per (tool, scanner, run), raw results and provenance kept for the Benchmarks tab.
-- bench_runs: one row per gate/report run; gate jsonb holds the B0-B8 report (see docs/BENCH_GATE.md).

CREATE TABLE IF NOT EXISTS tool_benchmarks (
    id              bigserial PRIMARY KEY,
    tool_id         bigint NOT NULL REFERENCES tools(id) ON DELETE CASCADE,
    scanner         text   NOT NULL,
    scanner_version text   NOT NULL DEFAULT '',
    image_digest    text   NOT NULL DEFAULT '',
    ran_at          timestamptz NOT NULL DEFAULT now(),
    duration_ms     bigint NOT NULL DEFAULT 0,
    exit_code       int    NOT NULL DEFAULT 0,
    verdict         text   NOT NULL,
    skip_reason     text   NOT NULL DEFAULT '',
    raw             jsonb,
    run_id          bigint
);

CREATE INDEX IF NOT EXISTS tool_benchmarks_tool_idx ON tool_benchmarks (tool_id, scanner, ran_at DESC);
CREATE INDEX IF NOT EXISTS tool_benchmarks_run_idx ON tool_benchmarks (run_id);

CREATE TABLE IF NOT EXISTS bench_runs (
    id          bigserial PRIMARY KEY,
    sha         text NOT NULL DEFAULT '',
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    gate        jsonb
);
