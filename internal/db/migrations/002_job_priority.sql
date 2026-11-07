-- Popular tools are verified first: if the budget runs out mid-pass, the long tail waits.
ALTER TABLE jobs ADD COLUMN priority INT NOT NULL DEFAULT 0;
DROP INDEX IF EXISTS jobs_claim;
CREATE INDEX jobs_claim ON jobs (stage, priority DESC, run_after, id) WHERE state = 'queued';
