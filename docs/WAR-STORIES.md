# War Stories

Incidents and near-misses from building and operating DistTaskQueue, written down because they are the most useful thing to talk about. Each one: what happened, the root cause, the fix, and the transferable lesson. Design rationale lives in [ARCHITECTURE.md](./ARCHITECTURE.md); this file is what went wrong.

Numbers worth having in your head: 5,000 jobs / 4 workers / 5.8 s in the loadtest; worker killed mid-job recovers in 21 s (13 s to detect); a 1,000-job flood scales workers 1→5 and drains in ~50 s; the whole system runs on one ~€8/mo box.

## 1. The pipeline was dead for weeks and every pod was green

During the DigitalOcean → Hetzner migration, the copied OAuth token failed with `invalid_grant`. Checking the old cluster: it had been failing identically, every 60 seconds, for an unknown number of weeks. Ten pods `Running`, 77 days of uptime, zero emails synced.

Root cause: the Google OAuth consent screen was in "Testing" mode, where Google expires refresh tokens after 7 days. The scheduler logged a WARN per poll and nothing else noticed.

Fix: re-run `oauth-setup`, publish the consent screen to production so tokens stop expiring.

Lesson: liveness probes measure the process, not the job. A pipeline needs a freshness signal — "seconds since last successful sync" — and something watching it. This system still doesn't have that alert; that's the honest open item, and saying so is more credible than pretending otherwise.

## 2. BRPOP can lose a job: the pop-to-claim window

Original design: `BRPOP` the job ID off the Redis list, then claim the row in Postgres with `UPDATE … WHERE status='queued'`. If the worker dies between those two steps, the ID is gone from Redis and the row sits `queued` with nothing coming for it except a 60-second stale-sweep backstop.

Fix: the reliable-queue pattern. `BLMOVE` the ID into a per-worker `processing:<stage>:<worker_id>` list instead of popping it, `LREM` (ack) only after the job's row is settled in Postgres, and give the sweeper a pass that moves processing-list entries of dead workers (heartbeat gone) back onto the stage queue. Both recovery paths can push duplicate IDs; the conditional `UPDATE` claim — the system's only mutex — absorbs them: one worker wins, the rest ack and drop.

Lesson: at-least-once delivery plus an idempotent claim beats chasing exactly-once. Know where your redelivery comes from and make the claim the single point of truth.

## 3. The fix that nearly shipped a worse bug

The BLMOVE change required moving heartbeats from per-job goroutines to one process-lifetime loop (the sweeper must be able to judge a worker that died *between* pop and claim, when no job-scoped heartbeat exists).

That silently broke an invariant the orphan-revival pass depended on: "live heartbeat ⇒ the claimed job is being worked." Under process-lifetime heartbeats, a transient Postgres error after a claim — `MarkDone` or `AdvanceJob` fails once, worker survives and moves on — would strand the job in `running` forever, because its owner's heartbeat never expires. The old per-job design self-healed this exact fault. Adversarial review caught it before merge.

Fix: fail-stop. Any post-claim settle failure exits the worker process (`errPostClaim`). The pod restarts, the heartbeat expires, and the ordinary recovery path picks the job up. One carve-out: if the settle failed because the context was cancelled (SIGTERM during a deploy), exit clean instead of dirty.

Lesson: crash-only design — when in doubt, die and let recovery run; it's the one code path that's always tested. And when you refactor, enumerate the invariants your recovery machinery assumes, because the type checker won't.

## 4. The autoscaler saw the burst; our chart didn't

First flood demo: 1,000 jobs enqueued, KEDA scaled workers 1→5, and the queue-depth chart was a flat zero. The `/metrics` gauge is refreshed by a 10-second cache loop; the burst drained in about ten seconds and fell entirely between two refreshes. KEDA polls Redis `LLEN` directly, so it reacted to a spike our own observability never recorded.

Fix: made flood jobs 250 ms instead of 10 ms so the demo load outlives the metric's resolution, and documented the gauge lag in the runbook.

Lesson: every metric has a freshness; a cached gauge cannot represent states shorter than its refresh interval. When a dashboard and an autoscaler disagree, check who reads the source and who reads a cache.

## 5. KEDA couldn't find Redis

The ScaledObjects used `address: redis:6379` — the same hostname every app pod uses. No HPA appeared; `kubectl describe` said `lookup redis: no such host`. The KEDA operator runs in the `keda` namespace and resolves DNS from there, not from the namespace the ScaledObject lives in.

Fix: `redis.disttaskqueue.svc.cluster.local:6379`.

Lesson: an address in a CRD is resolved by whichever controller reads it. Cross-namespace anything in Kubernetes deserves a fully-qualified name.

## 6. Secrets before StatefulSets, or postgres initializes with an empty password

From the first cloud bring-up: applying manifests before creating `dtq-secrets` means postgres starts with `POSTGRES_PASSWORD` resolving to empty, and `initdb` bakes that in. Creating the Secret afterwards does not re-initialize the database — every worker then fails with "password authentication failed" and nothing obvious says why.

Fix at the time: `ALTER USER dtq WITH PASSWORD …` and restart the app pods. Prevention: the runbook now applies secrets first and says the order is load-bearing.

Lesson: initialization-time configuration is a one-shot read. Apply order matters exactly when some component consumes config once and never again.

## 7. Kubernetes injected an env var that crashed Gotenberg

Gotenberg crashlooped with `invalid overriding value 'tcp://…' from API_PORT`. Nobody set `API_PORT`. Kubernetes did: service links auto-inject `<SERVICE>_PORT` env vars into every pod, and our `api` Service produced an `API_PORT=tcp://…` that Gotenberg read as its own listen-port config.

Fix: `enableServiceLinks: false` on the Gotenberg pod spec.

Lesson: in Kubernetes your container's environment is not fully yours. Any app that reads generic env names can collide with service links.

## 8. The cloud swap that was boring on purpose

Moving DigitalOcean → Hetzner took about 20 minutes for real: provision, curl-install k3s, secrets (carrying `TOKEN_ENCRYPTION_KEY` — without it every stored OAuth token decrypts to noise), a targeted `pg_dump` of `users` + `oauth_tokens` only (queue state deliberately starts empty; the History cursor re-seeds itself), `kubectl apply`, verify.

It was boring because of decisions made long before: 12-factor config, one image for all binaries, provider-agnostic manifests, Postgres as the single source of truth. The only incident was the dead token (story 1), which the migration surfaced rather than caused.

Lesson: portability isn't a migration project, it's the accumulated absence of hardcoding. State with one home moves easily.

## Decisions to speak to

The full reasoning is in ARCHITECTURE.md; headline answers:

- **Why hand-rolled instead of Asynq/River/SQS** — the point is owning the primitives (§1).
- **Why Postgres is the source of truth and Redis is just a queue** — Redis is bad at authoritative state; survive Redis loss via re-push (§3.2).
- **Why heartbeats + a polling sweeper, not Redis keyspace notifications** — expiry events aren't guaranteed delivery; you'd need the catch-up scan anyway (§3.3).
- **Why three worker pools, not one** — fetch is Gmail-I/O-bound, render is Chromium-CPU-bound, upload is Drive-I/O-bound; independent scaling and surgical retries (§3.1).
- **Why a conditional UPDATE is the only lock** — one mutex, everything else at-least-once + idempotent (§6.1).
- **Why BLMOVE processing lists** — story 2 (§5.7).
- **Why KEDA over prometheus-adapter** — scales on Redis `LLEN` directly, no Prometheus stack to run (OPERATIONS §13).
- **What it replaces** — a cloudhq "save emails to Drive" subscription; same job, one cheap VM, and every failure mode is now mine to understand.
