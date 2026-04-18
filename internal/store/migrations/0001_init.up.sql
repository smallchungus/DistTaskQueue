CREATE TABLE pipeline_jobs (
  id              UUID PRIMARY KEY,
  stage           TEXT NOT NULL,
  status          TEXT NOT NULL,
  payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
  worker_id       TEXT,
  attempts        INT NOT NULL DEFAULT 0,
  max_attempts    INT NOT NULL DEFAULT 8,
  last_error      TEXT,
  next_run_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  claimed_at      TIMESTAMPTZ,
  completed_at    TIMESTAMPTZ,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX pipeline_jobs_stage_status_idx ON pipeline_jobs (stage, status);
CREATE INDEX pipeline_jobs_queued_next_run_idx ON pipeline_jobs (status, next_run_at) WHERE status = 'queued';

CREATE TABLE job_status_history (
  id            BIGSERIAL PRIMARY KEY,
  job_id        UUID NOT NULL REFERENCES pipeline_jobs(id) ON DELETE CASCADE,
  stage         TEXT NOT NULL,
  from_status   TEXT,
  to_status     TEXT NOT NULL,
  worker_id     TEXT,
  error         TEXT,
  at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX job_status_history_job_at_idx ON job_status_history (job_id, at);
