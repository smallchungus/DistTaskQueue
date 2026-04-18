# Architecture

This document explains **what** each piece of DistTaskQueue does, **why** the design looks the way it does, and **what alternatives were considered and rejected**. The goal is that a reader who has never touched the repo can, after reading this, walk an interviewer through any component in depth — or operate the system in production.

If you're looking for "how do I run it," start with [the README](../README.md). If you're looking for "how do I operate / migrate / scale it," see [OPERATIONS.md](./OPERATIONS.md).

---

## 1. What this is

A distributed task queue, hand-rolled in Go, that powers a continuous Gmail → PDF → Google Drive sync. Every email landing in the primary inbox is rendered to PDF and uploaded to Drive (with attachments preserved) inside a dated folder tree.

It is simultaneously:

- A **real personal tool** — replaces the author's cloudhq subscription.
- A **portfolio artifact** — demonstrates distributed-systems primitives (multi-stage pipeline, claim atomicity, heartbeat-based failure recovery, exponential-backoff retries, at-least-once delivery with idempotent side effects) on real infra (Kubernetes, Postgres, Redis, Cloudflare Tunnel).

The "hand-rolled" part is deliberate. A library like Asynq, Machinery, or River would handle 90% of this in a few dozen lines, but would also hide every design decision behind someone else's abstractions. The trade is real engineering breadth over shortest time-to-ship.

---

## 2. The one-paragraph explanation

A **scheduler** polls Gmail every 5 minutes. New messages become rows in a `pipeline_jobs` table (Postgres, source of truth) and IDs on a Redis list (queue). Stateless **workers**, one pool per stage (fetch → render → upload), `BLPOP` an ID off the queue, atomically claim the job row via `UPDATE … WHERE status='queued' RETURNING`, process it, and either advance to the next stage or mark it done. Workers write a short-lived heartbeat key to Redis every 5 s while processing. A **sweeper** scans once every 5 s for (a) running jobs whose worker heartbeat expired → requeue, (b) queued jobs whose backoff elapsed → re-push to Redis, (c) queued jobs without `last_error` that have been sitting too long → re-push (recovers `BLPOP`-to-claim crashes). An **HTTP API** serves health + version endpoints and a live dashboard; a `/metrics` endpoint exposes Prometheus gauges for queue depth and job counts, set up to feed a future HPA. An **oauth-setup** one-shot CLI bootstraps the encrypted Google OAuth token into Postgres. All binaries build from the same Go image; Kubernetes manifests under `deploy/k8s/` apply unchanged to any k3s cluster (currently DigitalOcean today, Hetzner tomorrow).

---

## 3. Why this shape

Three design decisions pinned the architecture. Everything else is a consequence.

### 3.1 Why a multi-stage pipeline instead of one monolithic worker

The email workflow has three clearly different resource profiles:

| Stage | Bottleneck | What gets expensive |
|---|---|---|
| `fetch` | Gmail API I/O | Network latency, Gmail rate limits |
| `render` | Chromium CPU | PDF rendering is single-threaded CPU |
| `upload` | Drive API I/O | Drive folder lookups + upload bandwidth |

A monolithic worker that does all three ties the scaling characteristics together. Doubling `render` capacity to handle a rendering-CPU spike would also double `fetch` and `upload` pods, which are not the bottleneck. Separating the stages means each pool's replica count can move independently, which is what an HPA on queue depth actually exploits.

A secondary benefit: surgical retries. A 503 from the Drive upload API shouldn't re-fetch a 10 MB email from Gmail, or re-run Chromium on it. Today, if an `upload` stage fails, only the upload retries — the PDF and attachments already exist on the persistent volume and are reused.

### 3.2 Why Postgres is the source of truth and Redis is a cache-like queue

Redis is an excellent queue: `LPUSH` + `BLPOP` is a literal textbook producer/consumer pattern, latency is microseconds, and horizontal reads scale linearly. What Redis is **bad** at is authoritative state. A job claimed from Redis and then processed by a worker that dies before reporting back has no durable record that it ever existed.

Postgres fixes that. Every job is a `pipeline_jobs` row created by the scheduler before the Redis push. Claims are `UPDATE pipeline_jobs SET status='running', worker_id=…, claimed_at=now() WHERE id=$1 AND status='queued' RETURNING id` — atomic by Postgres's row-level locking. The Redis list holds only the *job ID*; the job itself lives in Postgres and survives any Redis scenario.

The consequence: the system can tolerate Redis losing its in-memory queue (e.g., a pod restart without persistence) as long as the sweeper can find `status='queued'` rows and re-push them. It does. That's the "BLPOP-to-claim race" recovery path explained later.

Alternative rejected: Redis streams with consumer groups. Streams give you durability (optional AOF persistence), consumer groups, and per-message ACKs. Functionally they solve the same problem. Two reasons I didn't use them:

1. The whole value of the project is showing I can build the primitives by hand on a simpler substrate. Streams already encode the pattern; lists + a Postgres row make the pattern visible in the code.
2. A single shared Postgres is a more honest source of truth than split Redis-plus-durable-queue plus Postgres-for-other-state. One source of truth is easier to reason about.

### 3.3 Why heartbeats instead of Redis key expiry notifications

When a worker dies mid-job, we need to detect it and requeue. Two options:

- **Heartbeat**: worker writes `SET heartbeat:<worker_id> now() EX 15s` every 5 s while processing. TTL > 2× interval so a single missed write doesn't expire. A separate sweeper polls for jobs with `status='running'` and asks Redis `EXISTS heartbeat:<worker_id>`; if missing, requeue.
- **Key expiry notifications**: Redis can push events when keys expire, via `notify-keyspace-events`. We'd set a "job-in-flight" key with a TTL, and a listener would react to the expiry event by requeuing.

Key expiry notifications are elegant but Redis docs are explicit: *they are not guaranteed delivery*. If the subscriber is disconnected at the moment the expiry fires, the event is lost. You'd need a periodic catch-up scan anyway. So you end up building both paths.

Heartbeats + polling is a single code path that handles the same correctness. Loses the "push" elegance, gains clarity.

---

## 4. End-to-end data flow: one email

Walking through what happens from Gmail to Drive. Referencing real code where relevant.

### Step 1 — Scheduler detects the email

`cmd/scheduler` runs a loop with a 5 min ticker (`internal/scheduler/scheduler.go`). Each tick calls `PollOnce(ctx)`:

```go
users, _ := store.ListUsers(ctx)            // one user today, N eventually
for _, u := range users {
    cursor, _ := store.GetGmailSyncState(ctx, u.ID)
    if cursor == "" {
        // First-ever poll for this user: seed from current Gmail state, no backfill.
        profile := gmail.Users.GetProfile("me").Do()
        store.SetGmailSyncState(ctx, u.ID, profile.HistoryId)
        continue
    }
    ids, newCursor, _ := client.LatestMessageIDs(ctx, cursor)
    for _, msgID := range ids {
        job, _ := store.EnqueueJob(ctx, NewJob{
            Stage: "fetch", UserID: &u.ID, GmailMessageID: &msgID,
        })
        queue.Push(ctx, "fetch", job.ID.String())
    }
    store.SetGmailSyncState(ctx, u.ID, newCursor)
}
```

`LatestMessageIDs` (`internal/gmail/client.go`) calls Gmail's `users.history.list` with `HistoryTypes("messageAdded", "labelAdded")` and `LabelId("INBOX")`. It filters results to `CATEGORY_PERSONAL` (the Primary tab) and deduplicates across both event types. The `labelAdded` inclusion is deliberate — it's what makes "moved from Spam to Inbox" also trigger a sync, not just new arrivals.

**Forward-sync only.** On the very first poll for a user, the scheduler seeds the cursor from the profile's current `historyId`. This means we never backfill the existing inbox. Rationale: (a) for a new user, backfilling an entire inbox could be tens of thousands of messages and would effectively DoS the Gmail API on our own quota, (b) the spec says "forward-sync" and demanding backfill would make first-time setup a multi-hour affair. If you want an older email synced, you can re-enqueue it manually with one SQL statement.

### Step 2 — Fetch worker retrieves the raw MIME

`worker --stage=fetch` runs `internal/handler.FetchHandler.Process()`. The worker framework (`internal/worker/worker.go`) does the same dance for every stage:

1. `queue.BlockingPop(ctx, "fetch", 30s)` → get a job ID off `queue:fetch`. BLPOP, not LPOP, so workers idle without burning CPU.
2. Parse the UUID, call `store.ClaimJob(ctx, id, workerID)`. This is the atomic step: the SQL is `UPDATE … WHERE id=$1 AND status='queued'`. If a race lost (two workers popped the same ID somehow, or the sweeper already promoted it elsewhere), rows-affected is 0 and we get `ErrJobNotClaimable` — log, drop, move on.
3. Spawn a heartbeat goroutine: `Queue.Heartbeat(ctx, workerID, 15s)` called every 5 s from a ticker, cancelled when the handler returns.
4. Call the stage's `Handler.Process(ctx, job)`. For fetch, that's:

```go
client := gmail.New(ctx, ...)                      // fresh per job, cheap
raw := client.FetchMessage(ctx, *job.GmailMessageID) // Users.Messages.Get(id).Format("raw") + base64url decode
os.WriteFile(<DataDir>/mime/<jobID>.eml, raw, 0600)
return "render", nil                                 // advance to next stage
```

5. Worker sees non-empty `nextStage`, calls `store.AdvanceJob(jobID, "render")` which does `UPDATE … SET stage='render', status='queued', worker_id=NULL`. Then `queue.Push(ctx, "render", jobID)`.
6. Heartbeat goroutine cancelled. Loop back to BLPOP.

### Step 3 — Render worker builds the PDF

`worker --stage=render` runs `internal/handler.RenderHandler.Process()`:

```go
raw := os.ReadFile(<DataDir>/mime/<jobID>.eml)
parsed := parseMessage(raw)   // internal/handler/mime.go — walks multipart tree
```

`parseMessage` is the most delicate piece of handler code. Real Gmail messages are RFC 5322 with `multipart/alternative` (text + HTML), `multipart/mixed` (body + attachments), `multipart/related` (HTML + inline images), often nested. The parser walks the tree recursively:

- Leaf `text/html` → becomes `ParsedMessage.HTML` (prefer the first one seen).
- Leaf `text/plain` → becomes `ParsedMessage.Text`.
- Any part with `Content-Disposition: attachment` or a named non-text/multipart content-type → extracted into `ParsedMessage.Attachments`.
- Inline images (`Content-Disposition: inline`) without a filename → currently dropped. Adding them would require rewriting HTML `cid:` references to data URIs — out of scope; PDFs come out readable without them.
- `Content-Transfer-Encoding: base64` → decoded; `quoted-printable` → decoded via `mime/quotedprintable`; everything else → passed through.

Each attachment is written to `<DataDir>/attachments/<jobID>/<sanitized-filename>`. `SanitizeFilename` strips path separators (`/`, `\`), null bytes, leading dots, and collapses runs, capping at 200 chars. This is the defense against a malicious email with a filename like `../../etc/passwd`.

The HTML body (or `<pre>`-wrapped text fallback) is wrapped with a thin header (From / Date / Subject), then POSTed to Gotenberg's `/forms/chromium/convert/html` endpoint as a multipart form. Gotenberg returns the PDF bytes, which go to `<DataDir>/pdf/<jobID>.pdf`.

Cross-stage metadata — subject, sender email, received-at timestamp, attachment filenames — gets serialized to `<DataDir>/meta/<jobID>.json`. The upload stage reads this to name the Drive folder. This is simpler than stuffing everything into the Postgres `payload` column and keeps the filesystem-based staging consistent: MIME, PDF, attachments, meta all flow through the same volume.

Return `"upload"`. Worker advances the job.

### Step 4 — Upload worker creates folders and pushes to Drive

`worker --stage=upload` runs `internal/handler.UploadHandler.Process()`:

```go
meta := readJSON(<DataDir>/meta/<jobID>.json)
client := drive.New(ctx, ...)

// Build the folder chain: [RootPath split by "/", then YYYY, MonthName, DayName, EmailFolderName]
folders := [
  "02_GmailBackup", "Gmail Backup",                      // from DRIVE_ROOT_PATH
  "2026", "April 2026", "18 April 2026 (Saturday)",      // DateTreeFolders(meta.ReceivedAt)
  "Re: project update - alice@example.com (10:30)",      // EmailFolderName(...)
]
parent := "root"  // Drive's alias for My Drive root
for _, name := range folders {
    if id, cached := folderCache.Get(userID, currentPath); cached {
        parent = id; continue
    }
    id, _ := client.EnsureFolder(ctx, parent, name)  // list-or-create
    folderCache.Set(userID, currentPath, id, 24h)
    parent = id
}

client.Upload(ctx, parent, "email.pdf", "application/pdf", pdfBytes)
for _, name := range meta.AttachmentNames {
    data := os.ReadFile(<DataDir>/attachments/<jobID>/<name>)
    client.Upload(ctx, parent, name, "application/octet-stream", data)
}
return "", nil   // terminal — worker calls store.MarkDone
```

`EnsureFolder` is `Files.List(Q("name='<name>' and '<parent>' in parents and mimeType='application/vnd.google-apps.folder'"))` then, if empty, `Files.Create({Name, Parents, MimeType})`. The Redis folder cache keeps us from calling the Drive API 6 times per email after the first one each day.

Return `""` (empty nextStage). The worker calls `store.MarkDone(jobID)`. Status flips to `done`, `completed_at` set. The email is fully processed.

### Step 5 — Dashboard sees the numbers move

The HTTP API (`cmd/api`) is connected to the same Postgres and Redis. `/api/stats` runs `SELECT status, count(*) FROM pipeline_jobs GROUP BY status` and `LLEN queue:<stage>` for each stage. `/api/jobs/recent` queries the 25 most recent rows. The dashboard (`internal/api/dashboard.html`) polls both every second, feeds a Chart.js line chart with queue depths over a 60-second window, and re-renders the jobs table.

---

## 5. Component reference

One section per binary / internal package. Each covers **what it does**, **why it looks like this**, and **what alternatives were rejected**.

### 5.1 `cmd/api`

**What:** HTTP server on `:8080`. Routes: `GET /healthz`, `GET /version`, `GET /` (dashboard HTML), `GET /api/stats`, `GET /api/jobs/recent`, `POST /api/demo/{burst,slow,flaky,flood}`.

**Why:** Two uses. (1) Kubernetes liveness/readiness probes hit `/healthz`. (2) The dashboard is a portfolio demo for anyone clicking the public URL — it needs to be visually live, not just a JSON endpoint.

**Dashboard design choices:**

- **Polling over SSE.** SSE or WebSockets would reduce network chatter, but a 1-second polling cycle against two endpoints totaling maybe 5 KB is negligible on the cluster side. Polling is simpler (no connection-lifecycle bugs), caches cleanly through the Cloudflare tunnel, and works through any proxy.
- **Chart.js via CDN, not a framework.** Vanilla JS + one script tag = no build step, no bundler, zero tooling. The dashboard is one HTML file plus one Go handler that embeds it via `//go:embed`. If you're reading this asking "what if you wanted to add a third page", the answer is "just write a second handler that returns HTML, or port to Svelte / React / htmx if scope grows."
- **Dark palette.** Not because dark is objectively better, but because the text-on-status-badge contrast works better on dark and the dashboard shows more dense numeric data than prose.

**Demo endpoints:**

The `/api/demo/*` endpoints enqueue synthetic jobs tagged `is_synthetic=true` onto a dedicated `test` queue. The `test`-stage worker runs `NoopHandler`, which only honors `sleepMs` and `failRate` in the payload — no Gmail, no Drive. This is the cost-protection story from the design spec made concrete: bots and random visitors hitting the demo buttons can never cause a Gmail API call, Drive upload, or actual data movement. The "worst realistic abuse outcome" is CPU saturation, not a bill.

Rate limiting (`internal/api/ratelimit.go`) is a per-IP token bucket: 1 request/sec sustained with burst 5, enforced in-memory. The `/api/demo/flood` endpoint has an additional global token bucket (1 call per 60 s) so a bot can't chain-fire 1000-job floods.

**Alternatives considered:**

- **Separate dashboard service on another port** — no reason; serving it from the same binary is simpler.
- **A real SPA (React, Svelte).** Overkill. Adds a build step and bundler that nothing else needs.
- **gRPC for internal routes.** No internal routes; everything is user-facing.

### 5.2 `cmd/worker`

**What:** The stateless processor. One binary, `--stage=<name>` flag picks which handler to run. `test` runs `NoopHandler`; `fetch`/`render`/`upload` run the corresponding `handler.*Handler`. Runs forever, popping jobs from its stage's queue.

**Why a single binary over five:**

Each stage has the same claim / heartbeat / advance / mark-failed plumbing. Duplicating that into five `cmd/worker-*` binaries would multiply maintenance. The stage-specific logic is a ~100-line handler; the rest is shared.

Downsides: the image is bigger than it strictly needs to be (every worker pod carries all 5 binaries on disk, though only one runs). It's measured in single-digit MBs, so it doesn't matter. If it ever does, split at that point.

**Lifecycle:**

```
for {
    ctx.Done ? return
    id := queue.BlockingPop(ctx, stage, PopTimeout)  // idle here
    if err == ErrEmpty { continue }
    job, _ := store.ClaimJob(ctx, id, workerID)
    if err == ErrJobNotClaimable { continue }   // race, not a problem
    hbCtx, cancel := ctx with context.Cancel
    go heartbeatLoop(hbCtx)                      // writes heartbeat key every 5s
    nextStage, err := handler.Process(ctx, job)
    cancel()                                     // stops the heartbeat goroutine
    if err != nil {
        store.MarkFailed(job.ID, err, now + backoff(attempts+1))
        continue
    }
    if nextStage != "" {
        store.AdvanceJob(job.ID, nextStage)
        queue.Push(ctx, nextStage, job.ID)
    } else {
        store.MarkDone(job.ID)
    }
}
```

**Graceful shutdown.** `ctx` in the outer loop is `signal.NotifyContext(ctx, SIGINT, SIGTERM)`. When Kubernetes sends SIGTERM (30 s before SIGKILL), the outer context cancels. In-flight handlers receive the cancel and can abort cleanly; BLPOP returns quickly because its own timeout is bounded by the context. The worker finishes the current job (at most PopTimeout + handler duration), then exits. Pods roll without job loss.

**Handler interface:**

```go
type Handler interface {
    Process(ctx context.Context, job store.Job) (nextStage string, err error)
}
```

The two-value return was a refactor during Phase 3E. The original interface returned only `error`; the worker always called `MarkDone` on success. That doesn't model a multi-stage pipeline — how do you advance a job to the next stage from inside a handler? The current shape: empty `nextStage` = terminal (MarkDone), non-empty = advance (AdvanceJob + Push to next queue). NoopHandler returns `("", nil)`, preserving the original single-stage semantics for synthetic jobs.

**Alternatives considered:**

- **Goroutine pool per worker.** Each worker pod could process N jobs concurrently via a semaphore channel. We don't. Today we scale horizontally by replicas, one pod = one in-flight job. Simpler claim/heartbeat reasoning. If we ever hit a bottleneck where the per-job overhead dominates handler work, a bounded semaphore is one line of code.
- **Queue-specific worker binaries.** Discussed above. Single binary wins on maintenance.

### 5.3 `cmd/sweeper`

**What:** Runs `SweepOnce` every 5 s. Three passes per sweep:

1. **Orphan revival.** `SELECT … WHERE status='running'`. For each row, check `EXISTS heartbeat:<worker_id>` in Redis. If absent, call `MarkFailed(id, "worker died", now())`. Status flips back to `queued` (or `dead` if max attempts reached).
2. **Delayed-retry promotion.** `SELECT … WHERE status='queued' AND last_error IS NOT NULL AND next_run_at <= now()`. For each, `queue.Push(stage, id)`. These are jobs that failed, backed off, and whose backoff just elapsed.
3. **Stale-queued revival.** `SELECT … WHERE status='queued' AND last_error IS NULL AND created_at < now() - interval`. Recovers jobs whose Redis push got consumed by a worker that crashed before `ClaimJob` — the worker BLPOPed the ID off the queue, then died, so the ID is gone from Redis but the row is still `queued` with no error. Sweeper re-pushes.

**Why three passes instead of a single unified query:**

Each pass has a different trigger predicate and a different action. Collapsing them into one query makes the SQL harder to read and the failure modes harder to reason about. Three short methods beat one clever one.

**Why 5 s instead of, say, 1 s or 60 s:**

Recovery latency = interval. At 5 s, worker-crash recovery is within ~20 s worst case (heartbeat TTL 15 s + sweep interval 5 s). Faster sweeps mean more Postgres load; 5 s is a cheap compromise that still looks responsive on the dashboard. Each sweep is a handful of indexed queries against `pipeline_jobs`, so the load is trivial until the table has tens of millions of rows.

**Duplicate pushes are safe.** If the sweeper pushes an ID that's already in Redis (e.g., a worker hasn't yet consumed it), two workers end up BLPOP-ing different ends of the queue and both try to claim. Postgres's `UPDATE … WHERE status='queued'` only succeeds for one; the other gets `ErrJobNotClaimable` and moves on. Wasted one round trip, no correctness loss.

### 5.4 `cmd/scheduler`

**What:** Iterates `users`, polls Gmail History API per user, enqueues fetch jobs for new INBOX/PRIMARY messages. 5-minute tick.

**Why a separate binary:**

It has a different rhythm (slow periodic polling) and dependencies (Gmail API, OAuth) than the workers. A shared binary with flag-gated behavior would conflate two concerns. One concern per binary.

**Why History API, not a full inbox scan:**

Gmail's `users.history.list` returns a log of changes since a given `historyId`. Pull the log, filter to message-added / label-added events under the `INBOX` label with `CATEGORY_PERSONAL`, enqueue. The cursor advances. This is O(changes-since-last-poll) instead of O(inbox-size) — critical for users with large inboxes.

**Why Pub/Sub push is deferred:**

Gmail supports Pub/Sub push notifications. Worth ~2 seconds of latency savings over 5-minute polling. Would require: a GCP Pub/Sub topic, a webhook endpoint (=needs a stable public URL with HTTPS = named Cloudflare Tunnel), and handling the periodic re-`watch()` call Gmail requires every 7 days. For a personal inbox, 5-minute polling is fine; for a multi-tenant product, reconsider.

### 5.5 `cmd/oauth-setup`

**What:** One-shot CLI. Prints the Google OAuth consent URL, listens on `http://localhost:8888/callback`, exchanges the auth code for a token, encrypts it with AES-GCM, saves to `oauth_tokens`.

**Why a separate binary:**

Bootstrapping OAuth requires a browser, which means the binary has to run on a machine with one — typically the user's laptop, not a Kubernetes pod. Running this as part of the api pod would either require shipping Chromium into the pod (no thanks) or having the pod serve a "click this link" flow, which is messier than a single-shot CLI.

**Why `http://localhost:8888` instead of `urn:ietf:wg:oauth:2.0:oob`:**

OOB (out-of-band) copy-paste flow was deprecated by Google in 2022; loopback is now the sanctioned Desktop OAuth pattern.

### 5.6 `internal/store` — Postgres layer

**What:** One struct (`Store`) wrapping a `*pgxpool.Pool`. Hand-written SQL for every method. Migrations embedded via `embed.FS` + golang-migrate.

**Why hand-written SQL, not an ORM:**

ORMs hide the query plan. For a queue, the performance of `ClaimJob` (`UPDATE … WHERE id=$1 AND status='queued' RETURNING …`) directly affects how many jobs/sec the system can sustain. You want to see that SQL, not trust a library to generate something equivalent. Same for `AdvanceJob`, `MarkFailed`, and the sweeper's three queries.

Second reason: ORMs typically load the Go struct on every read. The sweeper's list queries return only the columns it actually uses (`scanJobs` loads all 16, but that's a conscious choice). No reflection, no conversions, no surprises.

**Schema in two migrations:**

- `0001_init.up.sql` — `pipeline_jobs`, `job_status_history`. The queue-layer tables.
- `0002_users_oauth_sync_processed.up.sql` — `users`, `oauth_tokens`, `gmail_sync_state`, `processed_emails`. ALTERs `pipeline_jobs` to add `user_id`, `gmail_message_id`, `is_synthetic` — these stayed nullable because synthetic jobs don't have a user.

**Partial unique index for scheduler idempotency:**

```sql
CREATE UNIQUE INDEX ON pipeline_jobs (user_id, gmail_message_id)
  WHERE status NOT IN ('done', 'dead') AND is_synthetic = false
    AND user_id IS NOT NULL AND gmail_message_id IS NOT NULL;
```

The scheduler uses Gmail's `historyId` cursor, which shouldn't re-emit the same message. But if the scheduler retries a poll mid-cycle (bug, crash, Pod restart), it could produce duplicates. This index makes `EnqueueJob` reject the second attempt at insert time rather than us discovering the duplicate at upload time. Terminal rows (`done`, `dead`) are excluded so a user who wants to re-run a specific email can do so by manually updating the old row.

**What we deliberately don't do:**

- **Sharding.** Not needed until we have ~10M jobs/day.
- **Partitioning by time.** Easy to add later; no current retention policy.
- **A separate jobs-queue service.** `pipeline_jobs` is both the queue state and the audit log. Splitting them would duplicate state.

### 5.7 `internal/queue` — Redis layer

**What:** `LPush`, `BRPop`, `LLen`, plus `Heartbeat` (SET with TTL) and `IsWorkerAlive` (EXISTS). One list per stage, key format `queue:<stage>`.

**Why BRPOP instead of LPOP:**

BLPOP/BRPOP blocks on empty queues. Workers can idle waiting for a job without a busy-poll loop. Combined with `LPUSH` we get FIFO ordering — the oldest job comes out first, which matches user expectation (first in, first out).

**Why not Redis streams:**

Covered above. Lists are a simpler primitive; streams would carry durability semantics we don't use.

**Lua scripts (currently none):**

Not used yet. A prospective use is a fused "pop-and-heartbeat" script that atomically pops a job ID from the queue and writes the first heartbeat, closing an even tighter race than the stale-queued sweeper already handles. Considered, decided the sweeper's recovery path is sufficient for now.

### 5.8 `internal/worker` — framework code

Not to be confused with `cmd/worker` (the binary). This package provides the reusable claim / heartbeat / advance / retry plumbing that all stage handlers share. It also contains:

- `Compute(attempts) time.Duration` — exponential backoff with jitter. `min(2^attempts, 600) seconds`, scaled by 0.75–1.25 random multiplier. Capped at 10 min. After 8 attempts, `MarkFailed` flips to `dead`.
- `NewWorkerID()` — hostname + 8 bytes random hex. Hostname is sanitized (dashes stripped) so the downstream `SplitN` is unambiguous. Collisions across a single cluster are vanishingly unlikely.
- `NoopHandler` — for synthetic demo jobs. Reads `sleepMs` and `failRate` from payload, sleeps, optionally fails.

### 5.9 `internal/oauth` — AES-GCM + token storage

**What:** AES-GCM 256 encrypt/decrypt helpers, plus `LoadToken` / `SaveToken` wrappers that round-trip a `*oauth2.Token` through `store.OAuthToken`. Plus `NewSavingSource` — an `oauth2.TokenSource` wrapper that auto-persists refreshed tokens back to the DB.

**Why AES-GCM:**

Authenticated encryption. If an attacker tampered with the ciphertext in the DB, `Decrypt` fails with `cipher: message authentication failed`, not silently returning garbage that later behaves like a valid-but-wrong token.

**Why a separate package, not inlined in `gmail` / `drive`:**

Both Gmail and Drive share the same token (single Google OAuth client with both scopes). Without the shared package, each caller would need its own copy of the encrypt/decrypt + save-back wiring. This was refactored out of `internal/gmail` in Phase 3D.

**`NewSavingSource` wrapper:**

The oauth2 library refreshes tokens transparently — every call to `Token()` returns the current token, triggering a refresh if expired. But the refresh happens in memory; without a hook, the refreshed token is lost on process restart. The saving source wraps the library's TokenSource, detects when the returned access token differs from the previously-seen one, and saves the new token to Postgres. Idempotent — re-saving the same token is a no-op, so the wrapper is safe to call on every `Token()` request.

### 5.10 `internal/gmail` — Gmail client

**What:** `New(ctx, cfg)` builds an authenticated `*gmail.Service`. `LatestMessageIDs` walks the History API. `FetchMessage` does `Users.Messages.Get(id).Format("raw")` + base64url decode.

**Why the endpoint is configurable:**

Integration tests point it at an `httptest.Server`. The Google client library accepts `option.WithEndpoint(url)`, which replaces the base URL for the whole service. No real Google calls in tests, no mocking of HTTP internals.

**Why both `messageAdded` and `labelAdded`:**

Gmail's history log distinguishes between "an email was added to the INBOX label" (new mail) and "an existing email had the INBOX label added" (spam→inbox move, archive→inbox move, etc.). Filtering only on `messageAdded` missed the second case. Tracking both and deduplicating by message ID catches every scenario.

### 5.11 `internal/drive` — Drive client + folder cache

**What:** `New(ctx, cfg)` builds an authenticated `*drive.Service`. `EnsureFolder(parent, name)` is list-or-create. `Upload(parent, name, type, bytes)` is a simple multipart create. `FolderCache` wraps Redis for folder-ID memoization.

**Why the folder cache matters:**

Every email uploaded creates up to 6 folder levels: `02_GmailBackup / Gmail Backup / YYYY / Month YYYY / DD Month YYYY (Weekday) / <email-folder>`. The first five don't change across the day; the sixth is per-email. Without caching, every upload would issue ~6 list + 0–6 create API calls against Drive. With the cache, after the first email of the day, it's typically 1–2 calls (one for the email-specific folder, optionally one cached-miss for a new date level at midnight UTC).

**Why Redis for the cache, not in-memory:**

Multiple upload-worker replicas share the cache. If worker A created a folder, worker B should see the ID on its next lookup. In-memory per-worker would duplicate work and race on folder creation. Redis is already in the stack, adding a cache key is free.

**Why 24h TTL:**

Drive folders don't vanish on a schedule. 24h is long enough that we never re-resolve mid-session, short enough that a manually-deleted folder eventually gets re-created rather than erroring forever. Could be infinite; 24h is defensive.

### 5.12 `internal/pdf` — Gotenberg client

**What:** One method: `RenderHTML(ctx, html) ([]byte, error)`. Posts a multipart form with the HTML body to `/forms/chromium/convert/html`. Returns the PDF bytes from the response.

**Why Gotenberg over a Go-native library:**

Tried `go-wkhtmltopdf`, tried `gofpdf`, tried `chromedp` directly. Gotenberg wins for rendering real HTML — it's literally headless Chromium behind a small HTTP wrapper. CSS, web fonts, JavaScript (if enabled), embedded images all render faithfully. A Go-native library would produce PDFs that look like HTML from 1998.

Downsides: Gotenberg is a ~200 MB container, and each render spikes memory by 100–300 MB (Chromium processes). On the $12/mo DO droplet with 2 GB RAM, one concurrent render is fine; two would push us into swap. For now, render-worker has `replicas: 1` and per-worker concurrency is 1 (one goroutine at a time in the handler). If we need more throughput, we scale render-worker horizontally and let each replica render one at a time.

### 5.13 `internal/handler` — stage handlers

**What:** `FetchHandler`, `RenderHandler`, `UploadHandler`. Each implements the `worker.Handler` interface. Cross-stage state (MIME, PDF, attachments, meta) lives on the shared `/data` persistent volume.

**Why filesystem state instead of S3/GCS:**

Simpler. One PVC, one local path, accessible from every worker pod. No external object-storage dependency to provision, no credentials for another service. The downside is that the PVC is ReadWriteOnce (our k3s default storage class) — all three worker types must be scheduled to the same node, because a RWO PVC can only be mounted by pods on one node. Today with a single-node cluster this is invisible. For multi-node scale, either:

- Use a ReadWriteMany storage class (longhorn, NFS, or cloud-provider RWX volume).
- Replace the PVC with object storage (S3/GCS) via a small abstraction layer.

Documented in OPERATIONS.md's scaling section.

**Why cross-stage metadata on disk instead of in the Postgres `payload` column:**

The `payload` column is opaque JSON meant for caller-supplied parameters. Using it as a message-bus between stages would mix "input config" and "transient runtime data" into the same field. The filesystem path pattern mirrors how MIME and PDF already flow — meta is just another file alongside.

### 5.14 `internal/scheduler` — Gmail poller

Covered in Step 1 of the end-to-end walkthrough. Noteworthy detail not mentioned there: the initial poll seeds the cursor from `Users.GetProfile().HistoryId` via a bare `gmail.NewService` (no `gmail.Client` wrapper). Why: the `Client` wrapper loads + saves tokens through the oauth pkg, which is unnecessary overhead for a single profile read that doesn't modify state. We reuse the same oauth plumbing but skip the Client wrapper.

### 5.15 `internal/sweeper` — orphan + delayed revival

Covered in component §5.3.

### 5.16 `internal/api` — dashboard + demo + rate limit

Covered in §5.1.

### 5.17 `internal/store/migrations/` — schema

Covered in §5.6.

### 5.18 `internal/testutil` — testcontainers helpers

**What:** `StartPostgres(t)` and `StartRedis(t)` — each returns a ready-to-use client against a freshly-started container. Build-tagged `integration || loadtest` so they don't pull in Docker dependencies during unit-test runs.

**Why testcontainers over mocks:**

Mocking `pgx` or `go-redis` interfaces lets tests pass that wouldn't survive contact with real infrastructure. The claim atomicity test, for example, only exercises the right code path if `UPDATE … WHERE status='queued' RETURNING` actually executes in a real Postgres. A mock that "returns 1 row when status matches" would test our mock's behavior, not Postgres's.

Cost: integration tests are slower (each test spins a container, ~1–2 s overhead). On a modern laptop, the full integration suite runs in ~30 s. Acceptable.

### 5.19 `internal/loadtest` — reproducible benchmark

**What:** One test tagged `//go:build loadtest`, behind `make loadtest`. Enqueues 5000 NoopHandler jobs, spawns 4 in-process workers as goroutines, polls Postgres until `count(status=done) == 5000`, reports wall clock.

**Why in-process instead of cluster:**

The goal is proving the atomic primitives scale. Claim contention happens at Postgres and Redis, not in Go memory — it's the same whether the 4 workers are goroutines in one process or pods across 4 nodes. In-process is faster to run, portable to any environment with Docker, and doesn't require infra setup. Cluster-scale load testing belongs in OPERATIONS as a separate exercise for when we actually deploy multi-node.

---

## 6. Correctness guarantees

What the system promises, and how.

### 6.1 At-least-once delivery

**Promise:** Every Gmail message enqueued will eventually produce at least one successful Drive upload, as long as infrastructure stays reachable.

**How:** Three independent recovery paths:

1. Worker crash during handler → heartbeat TTL expires → sweeper re-queues (§5.3 pass 1).
2. Handler returns error → `MarkFailed` with `next_run_at = now + backoff`; sweeper promotes when backoff elapses (§5.3 pass 2).
3. Worker crash between `BLPOP` and `ClaimJob` → sweeper re-pushes after `STALE_QUEUED_THRESHOLD_SEC` (§5.3 pass 3).

**What breaks it:** Permanently losing both Postgres and Redis state simultaneously — the scheduler would re-poll Gmail with a lost cursor and re-enqueue from the current `historyId` forward, so strictly older emails in-flight at the moment of loss would be skipped. In practice both need durable volumes.

### 6.2 Idempotent side effects (not exactly-once)

**Promise:** The same email will produce exactly one Drive folder and one email.pdf, even under at-least-once delivery retries.

**How:**

- `processed_emails (user_id, gmail_message_id)` — unique index. Upload handler checks this table before uploading; if the email was already processed, marks the job done without re-uploading.
- `Files.Create` with a parent and name will create a *new* folder even if one with the same name exists. The `EnsureFolder` list-first-then-create pattern prevents duplicates by returning the existing folder when present.
- Drive file names are deterministic (`email.pdf` + sanitized attachment names). A double-upload from the same job would overwrite itself.

**What breaks it:** Renaming `02_GmailBackup` mid-session would cause the folder cache to point at a stale ID; EnsureFolder would create a new one with the original name. Minor inconvenience, not data loss.

### 6.3 Claim atomicity

**Promise:** A job is processed by exactly one worker at a time.

**How:** `ClaimJob` is a single `UPDATE … WHERE id=$1 AND status='queued' RETURNING id`. Postgres row-level locking guarantees that two concurrent `UPDATE`s on the same row serialize — only one succeeds, the other returns zero rows. The successful worker proceeds; the losing worker gets `ErrJobNotClaimable` and moves on.

**What breaks it:** Nothing within the design. If you bypassed `ClaimJob` and hand-edited `worker_id` on a `running` job, you'd break it — same as any consistency constraint you can violate with direct SQL.

### 6.4 Forward progress

**Promise:** Any given job either completes (`done`) or exhausts retries (`dead`) within a bounded time.

**How:** Max attempts is 8. Backoff is `min(2^attempts, 600)` seconds. Worst case: a persistently failing job takes 1 + 2 + 4 + 8 + 16 + 32 + 64 + 128 ≈ 255 seconds of backoff plus 8 handler invocations before going dead. Sweeper intervenes within 5 s of any state inconsistency.

---

## 7. Scalability

### 7.1 Horizontal scaling model

Each stage can scale independently by replica count. The key insight: all workers of a stage share one Redis queue and one Postgres claim arena, so adding a worker adds throughput until the shared resource saturates.

Expected bottleneck order (assuming real Google APIs aren't throttling):

1. Gotenberg CPU for `render` (each render is single-threaded Chromium).
2. Redis RTT for `BLPOP` on deep queues (microseconds per op, thousands per second achievable).
3. Postgres claim contention (hundreds per second on a single writer; horizontal Postgres needs read replicas + logical sharding, out of scope).

The loadtest (`make loadtest`) hit ~2,400 jobs/sec on a laptop with 4 in-process workers — limited by Postgres round-trips, not any particular layer's throughput.

### 7.2 Autoscaling on queue depth

Not wired today, but the pieces are in place:

- `/metrics` (Prometheus format) exposes `dtq_queue_depth{stage="..."}` and `dtq_job_count{status="..."}`.
- With a Prometheus scrape + prometheus-adapter in the cluster, an HPA can scale worker-render (for example) from 1 to N replicas when `dtq_queue_depth{stage="render"}` exceeds a threshold.
- Plan draft for this is in Phase 1.6 (deferred until a real cluster exists).

### 7.3 Multi-node limitation (known)

The shared `/data` PVC is ReadWriteOnce. For multi-node horizontal scaling of workers, either:

- Provision a ReadWriteMany storage class (longhorn, NFS, cloud RWX).
- Replace filesystem cross-stage staging with object storage (one-layer abstraction change).

Not today's problem. Documented in OPERATIONS.md §6.

---

## 8. Portability (hot-swap model)

### 8.1 What's cloud-agnostic

- Every binary is a 12-factor app: config via env vars, logs to stdout, no local state outside `/data`.
- K8s manifests use vanilla v1 APIs: Deployment, Service, StatefulSet, PersistentVolumeClaim, Secret, ConfigMap. No cloud-provider CRDs, no cloud-specific annotations.
- Image pulled from GHCR (public registry, accessible from any cloud).
- Data plane: Postgres + Redis run as StatefulSets with PVCs. Any k3s/k8s cluster with a default storage class works.
- Ingress: Cloudflare Tunnel daemon runs as a pod (or standalone process), no `LoadBalancer`/`Ingress` objects required. No assumed cloud-LB integration.

### 8.2 What's assumed

- A Kubernetes API server at some `https://<ip>:6443`, with a kubeconfig pointing at it.
- A `local-path` or equivalent `StorageClass` that provisions PVCs on the nodes.
- Network egress to `ghcr.io`, `accounts.google.com`, `gmail.googleapis.com`, `drive.googleapis.com`, `api.cloudflare.com`.

### 8.3 Swap procedure (concretely)

A full migration from DigitalOcean → Hetzner takes ~15 minutes end-to-end. See [OPERATIONS.md §3](./OPERATIONS.md) for the actual steps. Summary of what survives:

- **Application code** — zero changes.
- **K8s manifests** — zero changes (no cloud-specific fields).
- **Container images** — reused from GHCR.
- **Data** — Postgres dump + restore (~5 seconds for a small deployment), OR re-run the migration from scratch and have the scheduler re-seed from Gmail. For a personal deployment, the second option is often simpler.
- **OAuth tokens** — `TOKEN_ENCRYPTION_KEY` must carry over. If lost, the encrypted tokens in Postgres are unreadable and users must re-run `oauth-setup`.

---

## 9. Tradeoffs the system deliberately makes

Every one of these is a place I'd change my mind for a different project.

| Choice | Alternative | Why we didn't |
|---|---|---|
| Hand-rolled queue on Redis | Asynq / Machinery / River | Library hides the primitives; whole point is to demonstrate them. |
| Postgres source of truth | Redis streams with persistence | Two stores split state; harder to reason about. |
| Heartbeat + polling | Redis key expiry notifications | Notifications aren't delivery-guaranteed; you'd build polling anyway. |
| Single binary, `--stage` flag | One binary per stage | Five copies of the claim/heartbeat scaffold isn't worth independent versioning. |
| File system cross-stage staging | S3/GCS | Simpler; no external service. Known multi-node limitation accepted. |
| `messageAdded` + `labelAdded` filter | Pub/Sub push | Push needs a stable HTTPS webhook (named tunnel). 5-minute poll latency is fine for personal use. |
| `gmail.readonly` + `drive.file` scopes | `gmail.modify` + `drive` | Least-privilege; we don't need to edit Gmail or see other users' Drive files. |
| JSON logs to stdout | OpenTelemetry tracing | Tracing is useful for distributed request tracking; we have few user-facing endpoints. Metrics via Prometheus is the first observability layer added; tracing is deferred. |
| In-memory rate limiter | Redis-backed (sliding window) | Single api replica today; not worth the Redis round-trip per request. Trivial to swap if we go multi-replica. |
| Quick Cloudflare tunnel | Named tunnel + own domain | Quick tunnel is account-less, instant, fine for dev. Named tunnel follows once permanent hosting lands. |
| `ReadWriteOnce` PVC | `ReadWriteMany` / object storage | Default k3s storage class is RWO. Single-node deployment; no concurrent-node issue until we scale. |

---

## 10. What this system explicitly does NOT solve

- **Multi-tenant SaaS.** The data model supports many users, but there's no signup UX, no billing, no quota enforcement, no tenant isolation at the network layer. Today it's "you and anyone you add as a Google Cloud test user."
- **Exact-once delivery.** We guarantee at-least-once with idempotent side effects, which is strictly weaker. Exact-once in a distributed system requires consensus (Paxos/Raft) that isn't justified here.
- **Full inbox backfill.** Only forward-sync from the first poll onward. Intentional — see §4 Step 1.
- **Attachment OCR, inline-image rendering, pdf/a compliance.** Explicitly out of scope.
- **A real browser-app dashboard with auth.** The dashboard is public and unauthenticated by design (portfolio demo). Demo buttons are rate-limited so this isn't a liability.

---

## 11. Where to go next

Reading order for someone wanting to operate this:

1. [README.md](../README.md) — how to run it locally.
2. [OPERATIONS.md](./OPERATIONS.md) — runbooks: cloud swap, scale up, incident response.
3. `docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md` — the original design doc (spec-ish).
4. `docs/superpowers/plans/*.md` — per-phase implementation plans (history of how each chunk was built).
