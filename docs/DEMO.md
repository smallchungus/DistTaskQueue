# The 3-minute live demo

Rehearsed flow for interviews. Everything runs against production at
https://dtq.willchennn.com — no local setup. Have two terminals ready with
`KUBECONFIG=~/.kube/dtq-hetzner-config`.

**0:00 — Open the dashboard.** "This is a task queue I built from scratch in
Go on raw Redis and Postgres — no queue library. It backs up my Gmail to
Drive as PDFs, live, right now."

**0:20 — Press Flood.** Terminal 1: `kubectl -n disttaskqueue get hpa -w`.
"That just enqueued 1,000 jobs. KEDA polls the Redis queue depth every five
seconds — watch the replicas." Narrate 1→5 as it happens (~30 s).

**1:10 — Kill a worker mid-job.** Press Slow on the dashboard, then in
terminal 2: `kubectl -n disttaskqueue delete pod -l app=worker-test
--grace-period=0 --force`. "The worker was holding a job. Its heartbeat key
expires in 15 seconds, a sweeper notices, marks the job failed, and a fresh
pod retries it." Point at the dashboard when the job completes (~25 s).

**2:00 — The claim.** "Every recovery path here can deliver a job twice.
The only lock in the whole system is a conditional UPDATE in Postgres —
whoever wins the claim works the job, everyone else drops it. At-least-once
delivery plus an idempotent claim; exactly-once is a lie vendors tell you."

**2:30 — Close with the incident.** "The reason I trust all this: the
pipeline once died silently for weeks while every pod stayed green — dead
OAuth token. Now there's a freshness gauge in Grafana Cloud that pages me
if a poll hasn't succeeded in 30 minutes. Liveness probes measure the
process; this measures the job."

Numbers if asked: 5,000 jobs / 4 workers / 5.8 s loadtest; 21 s crash-to-done
(13 s detection); ~€8/month total infra. Deeper material: WAR-STORIES.md
(incidents), ARCHITECTURE.md (decisions and rejected alternatives).
