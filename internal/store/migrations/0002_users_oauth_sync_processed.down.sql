DROP INDEX IF EXISTS pipeline_jobs_user_message_unique;
DROP INDEX IF EXISTS pipeline_jobs_user_stage_status_idx;

ALTER TABLE pipeline_jobs
  DROP COLUMN IF EXISTS is_synthetic,
  DROP COLUMN IF EXISTS gmail_message_id,
  DROP COLUMN IF EXISTS user_id;

DROP TABLE IF EXISTS processed_emails;
DROP TABLE IF EXISTS gmail_sync_state;
DROP TABLE IF EXISTS oauth_tokens;
DROP TABLE IF EXISTS users;
