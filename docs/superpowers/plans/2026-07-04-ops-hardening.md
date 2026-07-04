# Ops Hardening Implementation Plan

**Goal:** Close the five operational gaps: no freshness signal, manual image deploys, no retention GC, no DB backups, exposed kube API. Plus Grafana Cloud free tier for the alerting.

**Approach notes (the karpathy/ponytail pass, kept in the plan):**
- Freshness reuses `gmail_sync_state.updated_at` — data that already exists — instead of a new Redis key or table. Requires deleting the `newCursor != cursor` guard so a successful-but-quiet poll still touches the row (one UPSERT/user/minute; negligible).
- Retention GC is a busybox CronJob running `find -mtime +7 -delete` — platform primitive, zero Go. This deviates from the spec's "check the job is done or dead first": a 7-day-old file for an unfinished job means the job is already dead or stuck past max_attempts, so age alone is a safe predicate. Spec gets amended first, per project rules.
- Backups are `pg_dump | gzip` to the same PV, 7-day rotation. Stated assumption: this protects against DB corruption/accidental deletion, NOT node loss (same disk). Off-box shipping is a later step if ever. `TOKEN_ENCRYPTION_KEY` goes to the password manager — that plus re-auth recovers from total node loss.
- Firewall is `hcloud firewall` — the real hole is 6443 (kube API) open to the world, not port 80. 80 stays open (rate-limited demo endpoints, dashboard is the portfolio).
- Grafana Cloud over self-hosted kube-prometheus-stack: the box has 4 GB; Alloy is ~1 pod vs ~1.5 GB of Prometheus+Grafana. Two alerts only. No dashboards-as-code, no Loki logs in v1 — YAGNI until an alert fires and logs are wanted.

## Task 1 — freshness gauge

Files: `internal/scheduler/scheduler.go`, `internal/api/metrics.go`, tests beside each.

1. Scheduler: replace the guarded cursor save with an unconditional one at the end of a successful `pollUser`. Test (integration, fake Gmail endpoint pattern already in `scheduler_integration_test.go`): poll twice with no new messages; assert `updated_at` advanced.
2. Metrics: add gauge `dtq_last_poll_success_timestamp_seconds` (unix seconds, 0 when no rows), refreshed alongside the others from `SELECT coalesce(extract(epoch from max(updated_at)), 0) FROM gmail_sync_state`. Test: seed sync state, hit `/metrics` via the router, poll-retry until the line appears with a plausible value.
3. Verify: `go test -race -count=1 -tags=integration ./internal/scheduler/... ./internal/api/...`
4. Commit `feat(scheduler,api): export last-successful-poll freshness gauge`; buildx push; rollout restart api + scheduler on the cluster; `curl /metrics | grep dtq_last_poll`.

## Task 2 — CI pushes the image

Files: `.github/workflows/ci.yml`.

1. `image` job: `needs: [lint, unit, integration]`, add `permissions: {contents: read, packages: write}`, `docker/login-action@v3` to ghcr with `GITHUB_TOKEN`, `push: ${{ github.event_name == 'push' }}`, tags `ghcr.io/smallchungus/disttaskqueue-api:latest` and `:sha-${{ github.sha }}`, platform linux/amd64. PRs keep building without pushing.
2. Verify: push, `gh run watch`, then `gh api /user/packages/container/disttaskqueue-api/versions | head` shows the new sha tag. Known trap: if GHCR returns 403, the package (created from a laptop token) must grant the repo write access under package settings → Manage Actions access.
3. Commit `ci: push image to GHCR on main`.

## Task 3 — retention GC CronJob

Files: `docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md` (§5.3 GC clause), `deploy/k8s/30-data-gc.yaml`.

1. Spec first: change "delete files older than 7 days whose corresponding job is `done` or `dead`" to age-only deletion with the rationale above.
2. CronJob: busybox, daily 04:20 UTC, mounts PVC `dtq-data`, runs `find` with `-mtime +7 -delete` over `/data/mime /data/pdf /data/attachments /data/meta` only (never `/data/backups`), then prunes empty dirs.
3. Verify: `make k8s-validate`; apply; `kubectl create job --from=cronjob/data-gc gc-smoke` and check logs + exit 0.
4. Commit `feat(k8s): daily retention GC for pipeline files`.

## Task 4 — nightly pg_dump CronJob

Files: `deploy/k8s/31-pg-backup.yaml`, `docs/OPERATIONS.md` §10.

1. CronJob: `postgres:16-alpine`, daily 03:50 UTC, `PGPASSWORD` from `dtq-secrets`, `pg_dump -h postgres -U dtq dtq | gzip > /data/backups/dtq-<date>.sql.gz`, rotate with `find -name '*.sql.gz' -mtime +7 -delete`, PVC mounted at `/data`.
2. OPERATIONS §10: replace "Not implemented today" with what runs, the restore one-liner (`gunzip -c … | psql`), the same-disk caveat, and the standing instruction to keep `TOKEN_ENCRYPTION_KEY` in a password manager.
3. Verify: `make k8s-validate`; apply; trigger once via `kubectl create job --from=cronjob/pg-backup backup-smoke`; `ls -la /data/backups` shows a non-trivial gzip.
4. Commit `feat(k8s): nightly pg_dump backup with 7-day rotation`.

## Task 5 — hcloud firewall

No repo files except OPERATIONS note.

1. `hcloud firewall create --name dtq` with inbound rules: tcp/22 from 0.0.0.0/0 + ::/0, tcp/80 from 0.0.0.0/0 + ::/0, tcp/6443 from this laptop's egress IP `/32` only. Everything else denied by default. Apply to server `dtq`.
2. Verify: `kubectl get nodes` still works from here (6443 allowed); `curl http://<ip>/healthz` still ok (80 open).
3. OPERATIONS: short §15 note — what the firewall allows and the one-liner to update the 6443 source when the home IP changes.
4. Commit `docs(ops): firewall runbook note`.

## Task 6 — Grafana Cloud free tier (needs user account)

User does in browser: sign up at grafana.com (free), open the stack, Connections → "Hosted Prometheus metrics" → via remote write; copy (a) the push URL, (b) the numeric username/instance ID, (c) generate an API token with metrics-write. Paste all three here.

Then:
1. `kubectl create ns monitoring`; Secret with the three values.
2. `helm install alloy grafana/alloy -n monitoring` with a minimal config: `prometheus.scrape` of `api.disttaskqueue.svc.cluster.local:80/metrics` every 60 s, forwarding to `prometheus.remote_write` with the Cloud credentials. One pod, ~100 MB.
3. Verify: Alloy pod Running; `dtq_queue_depth` visible in Grafana Explore within 2 min.
4. Two alert rules (click-steps for the user, or via API if they mint an editor service-account token):
   - `time() - max(dtq_last_poll_success_timestamp_seconds) > 1800` for 5 m → "Gmail sync stale >30 min" (the war-story-1 alert).
   - `sum(dtq_job_count{status="dead"}) > 0` for 10 m → "dead jobs present".
   Contact point: their email.
5. OPERATIONS §15 gets the monitoring runbook (what scrapes, where alerts live, how to rotate the token).
6. Commit `feat(k8s): ship metrics to Grafana Cloud via Alloy` + docs.

Execution order: 1 → 2 → 3 → 4 → 5 inline; 6 blocked on the user's Grafana signup (can land any time after 1).
