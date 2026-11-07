-- mcpkg schema v1
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- One row per MCP server (deduplicated across all sources).
CREATE TABLE tools (
  id               BIGSERIAL PRIMARY KEY,
  slug             TEXT NOT NULL UNIQUE,
  name             TEXT NOT NULL,
  title            TEXT NOT NULL DEFAULT '',
  description      TEXT NOT NULL DEFAULT '',
  version          TEXT NOT NULL DEFAULT '',
  repo_url         TEXT NOT NULL DEFAULT '',
  repo_subfolder   TEXT NOT NULL DEFAULT '',
  website_url      TEXT NOT NULL DEFAULT '',
  packages         JSONB NOT NULL DEFAULT '[]',
  remotes          JSONB NOT NULL DEFAULT '[]',
  sources          JSONB NOT NULL DEFAULT '{}',
  readme           TEXT NOT NULL DEFAULT '',
  readme_url       TEXT NOT NULL DEFAULT '',
  status           TEXT NOT NULL DEFAULT 'new',      -- new|active|unmaintained|blocked|missing|deprecated
  visibility       TEXT NOT NULL DEFAULT 'listed',   -- listed|hidden
  smoke_status     TEXT NOT NULL DEFAULT 'pending',  -- pending|works|needs_credentials|auth_required|broken|install_failed|skipped
  health           JSONB,
  security         JSONB,
  smoke            JSONB,
  enrich           JSONB,
  score            JSONB,
  install          JSONB,
  score_total      INT,
  grade            TEXT NOT NULL DEFAULT '',
  stars            INT NOT NULL DEFAULT 0,
  weekly_downloads BIGINT NOT NULL DEFAULT 0,
  last_commit_at   TIMESTAMPTZ,
  published_at     TIMESTAMPTZ,
  categories       TEXT[] NOT NULL DEFAULT '{}',
  summary          TEXT NOT NULL DEFAULT '',
  search           TSVECTOR GENERATED ALWAYS AS (
                     setweight(to_tsvector('simple', regexp_replace(coalesce(name, ''), '[^a-zA-Z0-9]+', ' ', 'g')), 'A') ||
                     setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
                     setweight(to_tsvector('english', coalesce(summary, '')), 'B') ||
                     setweight(to_tsvector('english', coalesce(description, '')), 'C')
                   ) STORED,
  first_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  verified_at      TIMESTAMPTZ
);
CREATE INDEX tools_search_idx   ON tools USING GIN (search);
CREATE INDEX tools_name_trgm    ON tools USING GIN (name gin_trgm_ops);
CREATE INDEX tools_categories   ON tools USING GIN (categories);
CREATE INDEX tools_listed_score ON tools (visibility, score_total DESC NULLS LAST);
CREATE INDEX tools_verified_at  ON tools (verified_at DESC NULLS LAST);

-- Dedupe keys: every package name, repo, registry name or remote URL maps to one tool.
CREATE TABLE identities (
  kind    TEXT NOT NULL,   -- registry|npm|pypi|oci|crate|repo|remote
  value   TEXT NOT NULL,
  tool_id BIGINT NOT NULL REFERENCES tools(id) ON DELETE CASCADE,
  PRIMARY KEY (kind, value)
);
CREATE INDEX identities_tool ON identities (tool_id);

-- Audit trail of every stage and hook run (powers the "Health" tab).
CREATE TABLE check_runs (
  id          BIGSERIAL PRIMARY KEY,
  tool_id     BIGINT NOT NULL REFERENCES tools(id) ON DELETE CASCADE,
  stage       TEXT NOT NULL,
  decision    TEXT NOT NULL,
  notes       TEXT[] NOT NULL DEFAULT '{}',
  result      JSONB,
  duration_ms INT NOT NULL DEFAULT 0,
  worker      TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX check_runs_tool ON check_runs (tool_id, created_at DESC);

-- Postgres-backed job queue (FOR UPDATE SKIP LOCKED). No Redis/SQS needed.
CREATE TABLE jobs (
  id           BIGSERIAL PRIMARY KEY,
  stage        TEXT NOT NULL,
  tool_id      BIGINT NOT NULL REFERENCES tools(id) ON DELETE CASCADE,
  payload      JSONB NOT NULL DEFAULT '{}',
  state        TEXT NOT NULL DEFAULT 'queued',   -- queued|running|done|dead
  attempts     INT NOT NULL DEFAULT 0,
  max_attempts INT NOT NULL DEFAULT 3,
  run_after    TIMESTAMPTZ NOT NULL DEFAULT now(),
  locked_by    TEXT,
  locked_until TIMESTAMPTZ,
  last_error   TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- At most one active job per (stage, tool): re-enqueueing is idempotent.
CREATE UNIQUE INDEX jobs_one_active ON jobs (stage, tool_id) WHERE state IN ('queued', 'running');
CREATE INDEX jobs_claim ON jobs (stage, run_after, id) WHERE state = 'queued';
CREATE INDEX jobs_lease ON jobs (locked_until) WHERE state = 'running';

-- Incremental crawl state per source (e.g. registry updated_since).
CREATE TABLE source_cursors (
  source     TEXT PRIMARY KEY,
  cursor     TEXT NOT NULL DEFAULT '',
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Human QA queue: sampled LLM output, high-severity findings, author claims.
CREATE TABLE review_queue (
  id          BIGSERIAL PRIMARY KEY,
  tool_id     BIGINT NOT NULL REFERENCES tools(id) ON DELETE CASCADE,
  reason      TEXT NOT NULL,
  details     JSONB,
  state       TEXT NOT NULL DEFAULT 'open',      -- open|approved|rejected|hidden
  reviewer    TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  resolved_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX review_one_open ON review_queue (tool_id, reason) WHERE state = 'open';

-- Tools submitted by authors via the website or `mcpkg add`.
CREATE TABLE submissions (
  id         BIGSERIAL PRIMARY KEY,
  url        TEXT NOT NULL,
  tool_id    BIGINT REFERENCES tools(id) ON DELETE SET NULL,
  state      TEXT NOT NULL DEFAULT 'received',   -- received|accepted|rejected
  error      TEXT NOT NULL DEFAULT '',
  client_key TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX submissions_client ON submissions (client_key, created_at DESC);
