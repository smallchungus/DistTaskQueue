CREATE TABLE users (
  id            UUID PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE oauth_tokens (
  user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider      TEXT NOT NULL,
  access_ct     BYTEA NOT NULL,
  refresh_ct    BYTEA NOT NULL,
  expires_at    TIMESTAMPTZ NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, provider)
);

CREATE TABLE gmail_sync_state (
  user_id       UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  history_id    TEXT NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE processed_emails (
  user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  gmail_message_id  TEXT NOT NULL,
  drive_folder_id   TEXT NOT NULL,
  processed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, gmail_message_id)
);

ALTER TABLE pipeline_jobs
  ADD COLUMN user_id           UUID NULL REFERENCES users(id) ON DELETE SET NULL,
  ADD COLUMN gmail_message_id  TEXT NULL,
  ADD COLUMN is_synthetic      BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX pipeline_jobs_user_stage_status_idx ON pipeline_jobs (user_id, stage, status);

CREATE UNIQUE INDEX pipeline_jobs_user_message_unique
  ON pipeline_jobs (user_id, gmail_message_id)
  WHERE status NOT IN ('done', 'dead') AND is_synthetic = false AND user_id IS NOT NULL AND gmail_message_id IS NOT NULL;
