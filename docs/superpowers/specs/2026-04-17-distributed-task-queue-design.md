# Distributed Task Queue — Design

Date: 2026-04-17
Status: Draft (awaiting user review)

## 1. Goal

Build a distributed task queue (Go, Redis, Postgres, Docker, k3s, GitHub Actions) that:

1. Powers a real workload — continuously syncs the user's Gmail primary inbox to Google Drive as PDFs (with attachments preserved). Replaces the user's current dependency on cloudhq.
2. Serves as a polished, FAANG-credible portfolio artifact — public GitHub repo, live hosted dashboard, and a real distributed-systems story (multi-stage pipeline, independent autoscaling, heartbeat-based failure recovery, at-least-once delivery, PostgreSQL-backed retries with exponential backoff).
3. Is multi-tenant by design even though only one user (the author) operates it day-to-day. Future Chrome-extension distribution must not require an architecture rewrite.

## 2. Scope

### In scope (v1)

- Multi-stage queue pipeline (`fetch` → `render` → `upload`), three independent worker pools.
- Continuous forward-sync of Gmail primary inbox (no backfill of existing archive).
- One PDF per email, attachments preserved as original files, grouped per email.
- Drive folder layout: `<root>/YYYY/MM/DD/<email-folder>/{email.pdf, <original attachments>}`.
- Heartbeat-based failure recovery (5 s heartbeat, 30 s sweeper threshold, at-least-once).
- Exponential backoff retries with retry count and last-error stored in Postgres.
- Live public dashboard with real-time job flow (SSE), demo trigger buttons (B+D), and a "flood" button to demonstrate HPA on queue depth.
- k3s on a single Hetzner CX22 (~$5/mo). Cloudflare Tunnel for public TLS without exposing the box.
- GitHub Actions: build, test, push images to GHCR, `kubectl apply` to the cluster.
- TDD throughout — tests for every unit before implementation. Integration tests against real Redis/Postgres via testcontainers-go (not mocks).

### Out of scope (v1)

- Multi-user signup UX, OAuth verification with Google for sensitive scopes (Gmail-read).
- Backfill of existing inbox archive.
- OCR on attachments.
- Email search / browse UI.
- Selective rules / per-label filtering. v1 syncs everything in primary inbox.
- Re-export / regeneration of previously processed emails.
- Mobile clients, Chrome extension. Architecture leaves room; build does not.
- Custom job types beyond Gmail→PDF→Drive. Job interface is generic enough to extend, but no second job type ships in v1.

### Open questions (please confirm or override before we start the implementation plan)

1. **Sync trigger:** Gmail History API polled every 5 min (simpler, ~5 min latency) vs. Gmail Pub/Sub push (real-time, requires public webhook + GCP Pub/Sub setup). **Recommendation:** polling for v1.
2. **PDF rendering:** Gotenberg as a sidecar deployment (battle-tested, Chromium under the hood). **Recommendation:** Gotenberg.
3. **Encryption of OAuth tokens at rest:** AES-GCM with key in k8s Secret. **Recommendation:** yes, even for single-user v1, because the tokens are valuable and the cost is small.
4. **Idempotency:** Postgres table `processed_emails (user_id, gmail_message_id) UNIQUE`. Workers check before doing work; on Drive upload, also use a content hash to skip exact re-uploads. **Recommendation:** yes.
5. **Retention of raw MIME and rendered PDF on the persistent volume:** delete after successful Drive upload, vs. keep N days for debugging. **Recommendation:** keep 7 days, then GC.

## 3. Architecture

```
                              ┌──────────────┐
                              │   Browser    │
                              │  (dashboard) │
                              └──────┬───────┘
                                     │ HTTPS via Cloudflare Tunnel
                                     ▼
                              ┌──────────────┐
       ┌─────────────────────▶│  API server  │◀── demo trigger buttons (B+D)
       │                      │  Go, net/http│
       │                      └──────┬───────┘
       │     ┌───────────────────────┼───────────────────────┐
       │     ▼                       ▼                       ▼
       │  ┌──────┐               ┌────────┐              ┌──────────┐
       │  │Redis │  queues +     │Postgres│  pipeline    │ Local PV │  raw MIME +
       │  │      │  heartbeats   │        │  state, OAuth│ (volume) │  rendered PDF
       │  └──────┘               └────────┘              └──────────┘
       │     ▲     ▲     ▲          ▲                      ▲       ▲
       │     │     │     │          │                      │       │
       │ ┌───┴─┐ ┌─┴───┐ ┌─┴────┐ ┌─┴─┐                 ┌──┴──┐ ┌──┴──┐
       │ │FETCH│ │REND │ │UPLOAD│ │SWP│                 │FETCH│ │UPLD │
       │ │pool │ │pool │ │pool  │ │EEP│                 │write│ │read │
       │ └──┬──┘ └──┬──┘ └──┬───┘ └─┬─┘                 └─────┘ └─────┘
       │    │       │       │       │
       │    ▼       │       ▼       │
       │ ┌─────┐    │    ┌──────┐   │
       │ │Gmail│    │    │Drive │   │
       │ │ API │    │    │ API  │   │
       │ └─────┘    │    └──────┘   │
       │            ▼                │
       │      (renders PDF locally)  │
       │                             │
       └─────────── scheduler ───────┘
       (polls Gmail History every 5 min,
        enqueues fetch jobs)
```

### Kubernetes deployments

| Deployment | Replicas | Job |
|---|---|---|
| `api` | 1 | HTTP API + dashboard SSE stream + demo trigger endpoints + OAuth callback |
| `scheduler` | 1 | Polls Gmail History API every 5 min; enqueues `fetch` jobs for new message IDs |
| `fetch-worker` | 1-5 (HPA on `queue:fetch` depth) | Pulls Gmail message via API, writes raw MIME to volume, enqueues `queue:render` |
| `render-worker` | 1-3 (HPA on `queue:render` depth) | Reads MIME, calls Gotenberg sidecar to render PDF, enqueues `queue:upload` |
| `upload-worker` | 1-5 (HPA on `queue:upload` depth) | Reads PDF + attachments, creates date-tree on Drive, uploads, marks done |
| `sweeper` | 1 | Scans worker heartbeats every 5 s; requeues jobs claimed by silent workers (>30 s) |
| `gotenberg` | 1 | PDF rendering sidecar (third-party container) |
| `redis` | 1 | StatefulSet, persistent volume |
| `postgres` | 1 | StatefulSet, persistent volume |

### Why three worker pools instead of one

- Different resource profiles. Fetch is IO-bound on Gmail; render is CPU-bound (Chromium); upload is IO-bound on Drive. Independent HPA lets each scale on its own bottleneck.
- Surgical retries. A 503 from Drive shouldn't re-fetch a 10 MB email from Gmail.
- Real distributed-pipeline story for the resume — three queues, three pools, three HPAs.
- Dashboard visualization is more interesting (cards flow stage to stage).

## 4. Data flow (one email, end to end)

1. **Scheduler tick (every 5 min):** Calls Gmail `users.history.list` with the saved `historyId` for the user. For each new `messagesAdded` whose label includes `INBOX` and category is `CATEGORY_PERSONAL`, inserts a row into `pipeline_jobs` with `stage='fetch', status='queued'` and pushes the job ID to `queue:fetch`. Updates the saved `historyId`.
2. **Fetch worker:** `BLPOP queue:fetch` with 30 s timeout. On hit:
   1. Atomically claim job in Postgres (`UPDATE pipeline_jobs SET status='running', worker_id=…, claimed_at=now() WHERE id=$1 AND status='queued' RETURNING …`).
   2. Begin heartbeat goroutine (writes to `heartbeat:<worker_id>` in Redis every 5 s with TTL 15 s).
   3. Fetch Gmail message in `format=raw`.
   4. Write MIME bytes to `/data/mime/<job_id>.eml`.
   5. Update job: `stage='render', status='queued'`. `LPUSH queue:render <job_id>`.
   6. Stop heartbeat. Worker loops back to BLPOP.
3. **Render worker:** Same claim+heartbeat pattern. Reads MIME, parses headers + HTML body, posts to Gotenberg `/forms/chromium/convert/html`, writes resulting PDF to `/data/pdf/<job_id>.pdf`. Extracts attachments from MIME, writes to `/data/attachments/<job_id>/`. Hands off to `queue:upload`.
4. **Upload worker:** Reads PDF + attachments. Resolves Drive folder path `<root>/YYYY/MM/DD/<email-folder>/` (creates folders idempotently via Drive API, caches folder IDs in Redis). Uploads PDF and each attachment. On success, sets `pipeline_jobs.status='done', completed_at=now()`. Inserts into `processed_emails (user_id, gmail_message_id)`. GC'd from disk by retention sweep.
5. **At any stage, if the worker crashes:** Heartbeat TTL expires. Sweeper finds the orphaned job (`status='running'` AND no live heartbeat), increments `attempts`, schedules requeue for `now() + backoff(attempts)`, sets `status='queued'`.

## 5. Data model

### 5.1 Postgres schema

```sql
-- Multi-tenant from day one. v1 has one row, but the column exists.
CREATE TABLE users (
  id            UUID PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Gmail + Drive OAuth tokens per user. Encrypted at rest.
CREATE TABLE oauth_tokens (
  user_id       UUID NOT NULL REFERENCES users(id),
  provider      TEXT NOT NULL,                   -- 'google'
  access_ct     BYTEA NOT NULL,                  -- AES-GCM ciphertext
  refresh_ct    BYTEA NOT NULL,
  expires_at    TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (user_id, provider)
);

-- Saved cursor for Gmail History API per user.
CREATE TABLE gmail_sync_state (
  user_id       UUID PRIMARY KEY REFERENCES users(id),
  history_id    TEXT NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The queue's source of truth. Redis lists are ephemeral; this table survives.
CREATE TABLE pipeline_jobs (
  id              UUID PRIMARY KEY,
  user_id         UUID NOT NULL REFERENCES users(id),
  gmail_message_id TEXT NOT NULL,
  stage           TEXT NOT NULL,                 -- 'fetch' | 'render' | 'upload'
  status          TEXT NOT NULL,                 -- 'queued' | 'running' | 'done' | 'dead'
  worker_id       TEXT,
  attempts        INT NOT NULL DEFAULT 0,
  last_error      TEXT,
  next_run_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  claimed_at      TIMESTAMPTZ,
  completed_at    TIMESTAMPTZ,
  is_synthetic    BOOLEAN NOT NULL DEFAULT false,  -- demo-trigger jobs; upload worker no-ops
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON pipeline_jobs (user_id, stage, status);
CREATE INDEX ON pipeline_jobs (status, next_run_at) WHERE status = 'queued';
-- Prevent the scheduler from creating two in-flight pipelines for the same message.
-- 'done' and 'dead' rows are excluded so reprocessing is allowed if needed.
CREATE UNIQUE INDEX ON pipeline_jobs (user_id, gmail_message_id)
  WHERE status NOT IN ('done', 'dead') AND is_synthetic = false;

-- Append-only history of every state transition. Powers dashboard timeline.
CREATE TABLE job_status_history (
  id            BIGSERIAL PRIMARY KEY,
  job_id        UUID NOT NULL REFERENCES pipeline_jobs(id),
  stage         TEXT NOT NULL,
  from_status   TEXT,
  to_status     TEXT NOT NULL,
  worker_id     TEXT,
  error         TEXT,
  at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON job_status_history (job_id, at);

-- Idempotency. Prevents duplicate Drive uploads if a job runs twice.
CREATE TABLE processed_emails (
  user_id           UUID NOT NULL REFERENCES users(id),
  gmail_message_id  TEXT NOT NULL,
  drive_folder_id   TEXT NOT NULL,
  processed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, gmail_message_id)
);
```

### 5.2 Redis keys

| Key | Type | Purpose |
|---|---|---|
| `queue:fetch`, `queue:render`, `queue:upload` | LIST | Per-stage job queues. Workers `BLPOP`. |
| `queue:delayed` | ZSET | Jobs waiting for backoff. Score = unix epoch of `next_run_at`. A small "promoter" loop in the sweeper moves due jobs into the per-stage list. |
| `heartbeat:<worker_id>` | STRING (TTL 15 s) | Worker liveness. Written every 5 s. Sweeper checks for absence. |
| `drive_folder_cache:<user_id>:<path>` | STRING | Cached Drive folder IDs to avoid round trips. |

Postgres is the source of truth for job claims (the `worker_id` column on `pipeline_jobs`, set in the same transaction as the status change). Redis holds queues and heartbeats only — no separate Redis claim key. Queue depth is observed directly via `LLEN` rather than maintained as a separate counter.

### 5.3 Filesystem layout (single PV)

```
/data/
  mime/<job_id>.eml
  pdf/<job_id>.pdf
  attachments/<job_id>/<original_filename>
```

GC by retention sweep: delete files older than 7 days whose corresponding job is `done` or `dead`.

### 5.4 Drive layout

```
<DriveSyncRoot>/
  2026/
    04/
      17/
        2026-04-17_103045_subject-slug_from-jane@example.com/
          email.pdf
          report.docx
          screenshot.png
```

Folder name is `<ISO date>_<HHMMSS>_<subject-slug>_<sender>` — bounded length, deterministic, sortable.

## 6. Dashboard / live demo

A single page, served by the API. No SPA framework — vanilla HTML + a small amount of JS, server-sent events for live updates. (Visual mockups TBD via the visual companion when we build it; this section is the functional spec.)

### Sections on the page

1. **Three pipeline columns, side by side: Fetch · Render · Upload.** Each column shows currently-running and recently-completed jobs as small cards. Cards animate left to right as jobs move from stage to stage. The number of replicas in each pool shows above the column ("3 workers").
2. **Queue depth line chart** for each stage, last 5 minutes.
3. **Worker liveness panel** — one tile per worker pod. Green if heartbeated within 10 s, yellow 10-30 s, red beyond.
4. **Demo trigger panel:**
   - `Enqueue 100 jobs` — fires 100 synthetic jobs through the pipeline
   - `Enqueue a flaky job` — synthetic job that fails 50% of the time, demonstrates backoff
   - `Enqueue a slow job` — runs for 30 s, demonstrates concurrency
   - `Kill a worker` — `kubectl delete pod` on a random worker, demonstrates heartbeat sweeper
   - `FLOOD (10K jobs)` — fires 10K synthetic jobs, demonstrates HPA scaling pods up
5. **Recent activity timeline** — last N completed real Gmail jobs, anonymized (sender domain only, no subject content).
6. **System info bar** — k3s node, Redis depth totals, Postgres job counts, uptime.

Synthetic and real jobs share the queue but are tagged so the dashboard can color them differently and so synthetic jobs are not actually written to Drive (the upload worker no-ops on `is_synthetic=true`).

### Rate limiting on demo buttons

Per-IP token bucket: 1 click/sec sustained, burst 5. The flood button is gated behind a 60 s cooldown. Prevents trivial abuse without needing accounts.

## 7. Failure handling

### Heartbeat sweeper

- Workers write `SET heartbeat:<worker_id> <ts> EX 15` every 5 s while a job is in flight. (TTL > 2× heartbeat interval.)
- Sweeper runs every 5 s. Query: `SELECT id, worker_id FROM pipeline_jobs WHERE status='running'`. For each, check `EXISTS heartbeat:<worker_id>`. If missing:
  - `UPDATE pipeline_jobs SET status='queued', attempts=attempts+1, next_run_at=now()+backoff(attempts), worker_id=NULL WHERE id=$1`.
  - Insert a `job_status_history` row.

### Backoff schedule

`backoff(attempts) = min(2^attempts, 600) seconds`, capped at 10 min. Jitter ±25%. After `attempts >= 8`, `status='dead'` and the job stops retrying. Dead jobs surface on the dashboard for manual inspection.

### Idempotency

- **Within the queue**: claim is keyed by Postgres `id`. The same job can run twice (at-least-once), but the upload worker checks `processed_emails` before doing the Drive write. If already processed, it skips and marks done.
- **At enqueue time**: scheduler uses Gmail `historyId` cursor; doesn't re-enqueue a message it has already enqueued because the cursor advances after each tick. Belt-and-suspenders: `pipeline_jobs` has a partial unique index on `(user_id, gmail_message_id)` for non-terminal, non-synthetic rows (see schema).
- **At Drive upload**: file existence check by name in the target folder before upload. Drive's resumable upload API also supports content hash dedup.

### What "at-least-once" means here

A given Gmail message will produce at least one Drive upload attempt. Because of the idempotency layer, it will result in exactly one Drive folder per email under normal operation. The contract is at-least-once delivery with idempotent side effects, not exactly-once delivery.

## 8. Deployment & ops

### Cluster

- Hetzner CX22 (€4.51/mo ≈ $5/mo). 2 vCPU, 4 GB RAM, 40 GB SSD.
- k3s installed via `curl -sfL https://get.k3s.io | sh -`. Real Kubernetes, single-node.
- One `local-path` PV class for Postgres, Redis, and the worker `/data` volume.
- Cloudflare Tunnel daemon as a Deployment, no `LoadBalancer` Service needed; no inbound port open on the box.

### Manifests

`deploy/k8s/` directory in the repo. One manifest per deployment plus shared ConfigMap and Secret. HPAs:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: { name: fetch-worker }
spec:
  scaleTargetRef: { kind: Deployment, name: fetch-worker }
  minReplicas: 1
  maxReplicas: 5
  metrics:
    - type: External
      external:
        metric: { name: redis_queue_depth, selector: { matchLabels: { queue: fetch } } }
        target: { type: AverageValue, averageValue: "20" }
```

Custom metrics from a small Prometheus + redis_exporter + prometheus-adapter stack. (Lightweight; runs in cluster.)

### CI/CD (GitHub Actions)

- On push to `main`:
  - `go test ./...` (race detector on)
  - `docker build` per image, push to GHCR with commit SHA tag
  - `kubectl set image deployment/...` for each updated image, via SSH to the Hetzner box (kubeconfig stored as repo secret)
- On PR: tests only.

### Secrets

- Google OAuth client ID/secret: k8s Secret, mounted into `api` and `scheduler` only.
- Token encryption key (AES-GCM 256-bit): k8s Secret, mounted into all workers and api.
- Cloudflare Tunnel credentials: k8s Secret, mounted into the tunnel deployment.

### Observability (v1, light)

- Structured JSON logs to stdout; surfaced via `kubectl logs`.
- Prometheus scrapes Redis depth, worker heartbeats, job counts. Scraped by the API, exposed on the dashboard.
- No Grafana, no alerting in v1. (Notify-on-dead-jobs can come later.)

## 9. Test strategy (TDD)

This project is built test-first. Every unit gets failing tests before implementation.

### Layers

1. **Unit tests** (`*_test.go` in same package). Pure logic: backoff calculator, folder-path derivation, MIME parsing, JSON shapes. Fast, no IO. Mocks limited to outbound Gmail/Drive HTTP clients (via interfaces).
2. **Integration tests** (`integration_test.go`, `//go:build integration`). Real Redis and Postgres via [testcontainers-go](https://golang.testcontainers.org/). Cover:
   - Worker claims a job and updates Postgres atomically.
   - Heartbeat sweeper requeues an orphaned job after the TTL expires.
   - Backoff progression across multiple failed attempts.
   - End-to-end pipeline (fetch stage → render stage → upload stage) with stubbed Gmail/Drive.
   - Idempotency: same `gmail_message_id` enqueued twice produces one Drive upload.
3. **End-to-end smoke test** (`make e2e`, runs in CI against an ephemeral kind cluster). Spins up the full stack, enqueues N synthetic jobs, asserts they all complete.
4. **Load test** (`make loadtest`, manual). Confirms 5K jobs across 4 workers in ~10 s. Validates the resume claim. Captured in repo as a reproducible artifact.

### What is NOT mocked

- Redis. Tests use real Redis via testcontainers.
- Postgres. Same. Mocking SQL queries is the kind of thing that lets bugs slip past.

### What IS mocked / stubbed

- Gmail API (HTTP client interface, fake server returns canned responses).
- Drive API (same).
- Gotenberg in unit tests of the render worker (HTTP client interface). In integration tests, a real Gotenberg container.

### Test pyramid target

Roughly 70/25/5 unit/integration/e2e by count. Integration tests are slower but catch the bugs that matter in a queue (race conditions, ordering, atomicity).

## 10. Success criteria

The project is "done" (v1 ships) when all of the following are true:

1. `git clone && docker compose up && make seed-token` gets a developer to a working local instance in <10 min.
2. The Hetzner box runs the system 24/7. The dashboard URL is public.
3. The author's primary inbox has been continuously syncing to Drive for at least 7 days with no manual intervention. Failures recover automatically.
4. The dashboard shows the resume-claim numbers reproduced live: kill a worker, watch the sweeper requeue. Click flood, watch HPA scale fetch-worker from 1 to 5. Click flaky-job, watch backoff retry succeed.
5. CI runs unit + integration tests on every PR. Green required before merge.
6. README contains: architecture diagram, quickstart, "why I built this," and a 60-second video showing the dashboard during a flood.
7. Test coverage: ≥80% on queue/scheduler/sweeper packages. Lower coverage acceptable on glue code.

## 11. Non-goals worth naming explicitly

- This is **not** trying to compete with Sidekiq, Asynq, or Temporal. It is a learning artifact + personal tool, intentionally hand-rolled where a library would be more practical.
- This is **not** a general-purpose hosted queue service. Other people cannot send their own jobs in v1. The "users" story is honest: one real user (the author) plus visitors who interact via the demo buttons.
- This is **not** trying to be exactly-once. At-least-once with idempotent side effects is the contract.
