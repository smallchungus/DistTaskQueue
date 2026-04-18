# Operations

Runbooks for operating DistTaskQueue in production. For *what* each component does and *why*, see [ARCHITECTURE.md](./ARCHITECTURE.md).

---

## 1. System inventory

What runs where, in the default deployment:

| Component | Binary / image | Replicas | Health |
|---|---|---|---|
| API | `ghcr.io/smallchungus/disttaskqueue-api:latest` (`/api`) | 1 | `GET /healthz` |
| Worker — fetch | same image, `/worker --stage=fetch` | 1 | (no HTTP, relies on heartbeat) |
| Worker — render | same image, `/worker --stage=render` | 1 | same |
| Worker — upload | same image, `/worker --stage=upload` | 1 | same |
| Worker — test (synthetic demo) | same image, `/worker --stage=test` | 0 or 1 | same |
| Sweeper | same image, `/sweeper` | 1 | same |
| Scheduler | same image, `/scheduler` | 1 | same |
| Gotenberg | `gotenberg/gotenberg:8` | 1 | `GET /health` |
| Postgres | `postgres:16-alpine` StatefulSet | 1 | `pg_isready` |
| Redis | `redis:7-alpine` StatefulSet | 1 | `redis-cli ping` |
| Cloudflare Tunnel | `cloudflared` on droplet (or as a Deployment) | 1 | tunnel connection count |

Every container-app pod pulls from GHCR. The `dtq-secrets` Kubernetes Secret holds `POSTGRES_PASSWORD`, `TOKEN_ENCRYPTION_KEY`, `GOOGLE_OAUTH_CLIENT_ID`, `GOOGLE_OAUTH_CLIENT_SECRET`.

---

## 2. Environment variables

Every binary is 12-factor. No file-based config.

### Shared

| Var | Default | Used by |
|---|---|---|
| `DATABASE_URL` | `postgres://dtq:dtq@localhost:5432/dtq?sslmode=disable` | all |
| `REDIS_URL` | `redis://localhost:6379/0` | all except oauth-setup |
| `TOKEN_ENCRYPTION_KEY` | required | worker (fetch/upload), scheduler, oauth-setup |
| `GOOGLE_OAUTH_CLIENT_ID` | required (except api / sweeper / worker-render) | worker, scheduler, oauth-setup |
| `GOOGLE_OAUTH_CLIENT_SECRET` | required (same) | same |

### api (`cmd/api`)

| Var | Default | Notes |
|---|---|---|
| `API_ADDR` | `:8080` | Listen address. |
| `DATABASE_URL` / `REDIS_URL` | empty | **If both empty, dashboard is disabled** (pod still serves `/healthz` and `/version`). Set both to enable `/`, `/api/stats`, `/api/jobs/recent`, `/api/demo/*`. |

### worker (`cmd/worker`)

| Var | Default | Notes |
|---|---|---|
| `--stage` flag | required | `fetch`, `render`, `upload`, or `test` |
| `DATA_DIR` | `/data` | Shared PV mount. Holds `mime/`, `pdf/`, `attachments/`, `meta/`. |
| `GOTENBERG_URL` | `http://gotenberg:3000` | Render-worker only. |
| `DRIVE_ROOT_FOLDER_ID` | empty (→ `root`) | Drive folder ID. Empty means "My Drive root." |
| `DRIVE_ROOT_PATH` | empty | Slash-delimited folders ensured under `DRIVE_ROOT_FOLDER_ID`. Example: `02_GmailBackup/Gmail Backup`. |

### sweeper (`cmd/sweeper`)

| Var | Default | Notes |
|---|---|---|
| `STALE_QUEUED_THRESHOLD_SEC` | `60` | How long a queued job without `last_error` can sit before the sweeper re-pushes it. |

### scheduler (`cmd/scheduler`)

Same required vars; no scheduler-specific tuning today. Poll interval is 5 min hardcoded.

### oauth-setup (`cmd/oauth-setup`)

| Var | Notes |
|---|---|
| `DATABASE_URL` | Required — target DB where the encrypted token will be written. |
| `GOOGLE_OAUTH_CLIENT_ID` / `SECRET` | Required. |
| `TOKEN_ENCRYPTION_KEY` | Required — same value the workers will use. |

Flags: `--email=wchen1396@gmail.com` — required, must match the Google account that will authorize.

---

## 3. Runbook — swap cloud providers

Time: ~15 min, assuming the new cluster is up.

### 3.1 On the new host (Hetzner, DO, Linode, …)

```bash
# 1. Create a VM (2+ GB RAM, 1+ vCPU, 30+ GB disk). Ubuntu 24.04.
#    Attach your SSH key at provision time.

# 2. Install k3s:
ssh root@<NEW_IP>
curl -sfL https://get.k3s.io | sh -
# Wait for node Ready:
kubectl get nodes

# 3. Copy kubeconfig to your laptop with the right server address:
ssh root@<NEW_IP> "cat /etc/rancher/k3s/k3s.yaml" \
  | sed "s|https://127.0.0.1:6443|https://<NEW_IP>:6443|" \
  > ~/.kube/dtq-new-config
chmod 600 ~/.kube/dtq-new-config
```

### 3.2 From your laptop

```bash
export KUBECONFIG=~/.kube/dtq-new-config

# Namespace + config (non-secret):
kubectl apply -f deploy/k8s/00-namespace.yaml -f deploy/k8s/01-config.yaml

# Image pull secret for private GHCR image:
kubectl -n disttaskqueue create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io \
  --docker-username=smallchungus \
  --docker-password="$(gh auth token)" \
  --docker-email=wchen1396@gmail.com

# Application secrets. PRESERVE TOKEN_ENCRYPTION_KEY from the previous cluster
# (kubectl -n disttaskqueue get secret dtq-secrets -o yaml on the OLD cluster,
# base64-decode, re-use here). Without it, existing oauth_tokens rows become
# unreadable and users must re-run oauth-setup.
kubectl -n disttaskqueue create secret generic dtq-secrets \
  --from-literal=POSTGRES_PASSWORD="$(openssl rand -base64 24)" \
  --from-literal=TOKEN_ENCRYPTION_KEY="<PREVIOUS_KEY>" \
  --from-literal=GOOGLE_OAUTH_CLIENT_ID="<real-client-id>" \
  --from-literal=GOOGLE_OAUTH_CLIENT_SECRET="<real-client-secret>"

# Everything else:
kubectl apply -f deploy/k8s/

# Wait for pods:
kubectl -n disttaskqueue rollout status statefulset/postgres
kubectl -n disttaskqueue rollout status statefulset/redis
kubectl -n disttaskqueue rollout status deployment/gotenberg
kubectl -n disttaskqueue rollout status deployment/api
kubectl -n disttaskqueue rollout status deployment/worker-fetch
# …etc
```

### 3.3 Migrate data (if keeping existing jobs / users)

```bash
# Dump from old cluster:
OLD_POD=$(KUBECONFIG=~/.kube/dtq-old kubectl -n disttaskqueue get pod -l app=postgres -o name)
KUBECONFIG=~/.kube/dtq-old kubectl -n disttaskqueue exec -i $OLD_POD -- \
  pg_dump -U dtq dtq > /tmp/dtq.sql

# Restore to new cluster:
NEW_POD=$(kubectl -n disttaskqueue get pod -l app=postgres -o name)
kubectl -n disttaskqueue exec -i $NEW_POD -- psql -U dtq -d dtq < /tmp/dtq.sql
```

If you DON'T migrate data, the scheduler's first poll on the new cluster initializes the Gmail sync cursor from the current `historyId`. Past emails are not re-processed; forward-sync resumes.

### 3.4 Ingress (Cloudflare Tunnel)

Quickest: log into the old droplet, `tmux kill-session -t cf`. On the new droplet, install `cloudflared` and run `cloudflared tunnel --url http://localhost:8080` (after `kubectl -n disttaskqueue port-forward svc/api 8080:80` in another tmux). The new `*.trycloudflare.com` URL starts serving.

Proper: named Cloudflare Tunnel. See §4.

### 3.5 Decommission the old cluster

```bash
# On the OLD host:
ssh root@<OLD_IP> "/usr/local/bin/k3s-uninstall.sh"
# On the cloud provider: delete the VM.
```

---

## 4. Runbook — set up a permanent public URL (named Cloudflare Tunnel)

Required: a Cloudflare account (free) and a domain you control (can use Cloudflare-registered domains or transfer DNS).

```bash
# One-time, on your laptop:
cloudflared tunnel login                              # opens browser
cloudflared tunnel create disttaskqueue               # creates tunnel, gives you a UUID
cloudflared tunnel route dns disttaskqueue dtq.yourdomain.com

# Creates ~/.cloudflared/<UUID>.json with credentials.

# Copy credentials file content into a k8s Secret:
kubectl -n disttaskqueue create secret generic cloudflared-tunnel \
  --from-file=credentials.json=$HOME/.cloudflared/<UUID>.json

# Deploy cloudflared as a pod:
cat <<'EOF' | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata: { name: cloudflared, namespace: disttaskqueue }
spec:
  replicas: 1
  selector: { matchLabels: { app: cloudflared } }
  template:
    metadata: { labels: { app: cloudflared } }
    spec:
      containers:
        - name: cloudflared
          image: cloudflare/cloudflared:latest
          args: ["tunnel", "--no-autoupdate", "--config", "/etc/cf/config.yaml", "run"]
          volumeMounts:
            - { name: config, mountPath: /etc/cf }
      volumes:
        - name: config
          secret:
            secretName: cloudflared-tunnel
            items:
              - { key: credentials.json, path: <UUID>.json }
              - { key: config.yaml, path: config.yaml }
EOF
```

Config file content (as a second key in the Secret):

```yaml
tunnel: <UUID>
credentials-file: /etc/cf/<UUID>.json
ingress:
  - hostname: dtq.yourdomain.com
    service: http://api:80
  - service: http_status:404
```

URL is now stable, TLS is terminated at Cloudflare's edge, no inbound port open on the box.

---

## 5. Runbook — scale workers up/down

```bash
# scale render-worker to 3 replicas
kubectl -n disttaskqueue scale deployment/worker-render --replicas=3

# check
kubectl -n disttaskqueue get pods -l app=worker-render
```

All workers of a stage share the same Redis queue and Postgres claim arena. Adding replicas adds throughput until the shared resource saturates (Gotenberg CPU for render; Drive API rate limits for upload; Gmail API rate limits for fetch).

Observe scaling effect on the dashboard at `https://<public-url>/`: queue depth drops faster; running-count goes up.

### Caveat: PVC is ReadWriteOnce

All worker pods must be scheduled to the same node because they share a RWO PVC. On a single-node k3s cluster this is automatic. On a multi-node cluster, you need to either:

- Set a `nodeSelector` on the worker Deployments to pin them to one node.
- Upgrade the storage class to RWX (longhorn, NFS, cloud-provider RWX volume).
- Replace the filesystem staging with object storage (~1-day engineering task; not done today).

---

## 6. Runbook — recover from a failed deployment

### Symptoms: api pod stuck in `ImagePullBackOff`

Check the imagePullSecret:

```bash
kubectl -n disttaskqueue get secret ghcr-pull -o json \
  | jq -r '.data[".dockerconfigjson"]' | base64 -d
```

If that's missing the current GHCR token or the token expired, recreate it:

```bash
kubectl -n disttaskqueue delete secret ghcr-pull
kubectl -n disttaskqueue create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io \
  --docker-username=smallchungus \
  --docker-password="$(gh auth token)" \
  --docker-email=wchen1396@gmail.com
kubectl -n disttaskqueue rollout restart deployment/api
```

### Symptoms: worker pods stuck in `CrashLoopBackOff`

```bash
kubectl -n disttaskqueue logs -l app=worker-fetch --tail=50
```

Most common cause: missing env var (e.g., `TOKEN_ENCRYPTION_KEY` not in `dtq-secrets`). Fix the Secret, pods self-recover on next restart.

### Symptoms: many jobs in `status='running'` but no heartbeats

Sweeper should be revive them. If sweeper is down:

```bash
kubectl -n disttaskqueue get pods -l app=sweeper
# If CrashLoop: check logs, fix, restart.
# If running but not sweeping: check for Postgres / Redis connection errors in logs.
```

### Symptoms: scheduler not enqueueing anything

```bash
kubectl -n disttaskqueue logs deployment/scheduler --tail=50
```

Likely causes:

- No users in the `users` table yet (run `oauth-setup`).
- OAuth token expired and refresh failed (the Google client auto-refreshes but needs a valid refresh token — re-run `oauth-setup`).
- Gmail API rate-limited (429 in logs — back off).

---

## 7. Runbook — first-time OAuth bootstrap

```bash
# On your laptop (needs browser access):
kubectl -n disttaskqueue port-forward svc/postgres 5432:5432 &

export DATABASE_URL='postgres://dtq:dtq@localhost:5432/dtq?sslmode=disable'
export GOOGLE_OAUTH_CLIENT_ID=<from dtq-secrets>
export GOOGLE_OAUTH_CLIENT_SECRET=<from dtq-secrets>
export TOKEN_ENCRYPTION_KEY=<from dtq-secrets>

go run ./cmd/oauth-setup --email=you@gmail.com
# Opens browser → authorize → saves encrypted token. Scheduler picks up within 5 min.
```

The `POSTGRES_PASSWORD` in `dtq-secrets` is the one in use; adjust the connection URL if it differs from `dtq:dtq`.

---

## 8. Runbook — delete a user / revoke access

```bash
kubectl -n disttaskqueue exec -it postgres-0 -- psql -U dtq -d dtq <<'SQL'
DELETE FROM users WHERE email = 'user@to-revoke.com';
SQL
```

The `oauth_tokens`, `gmail_sync_state`, `processed_emails`, and `pipeline_jobs` (for synthetic + that user's real jobs) all cascade on user delete via `ON DELETE CASCADE` / `ON DELETE SET NULL`. Workers will see missing user_id on in-flight jobs and error cleanly.

Browser-side revocation (so Google invalidates the refresh token): https://myaccount.google.com/permissions → DistTaskQueue → Remove access.

---

## 9. Runbook — metrics and dashboards

### 9.1 Prometheus scrape target

`api` serves `/metrics` in Prometheus format. Metrics:

- `dtq_queue_depth{stage="fetch|render|upload|test"}` — gauge, LLEN of each queue.
- `dtq_job_count{status="queued|running|done|dead"}` — gauge.
- `dtq_alive_workers` — gauge, count of live heartbeat keys.
- Go runtime metrics from `promhttp` (heap, GC, goroutines).

Sample Prometheus scrape config:

```yaml
scrape_configs:
  - job_name: disttaskqueue
    scrape_interval: 10s
    static_configs:
      - targets: ['api.disttaskqueue.svc:80']
```

### 9.2 Dashboards

Today: the built-in HTML dashboard at `/`. Queue-depth chart (Chart.js, 60 s window), job counts, live workers, recent jobs.

Future: Grafana dashboard fed by Prometheus. Not built; one-day task when we add Grafana to the cluster.

---

## 10. Backup & disaster recovery

Not implemented today. What you'd add for real prod:

- **Postgres:** logical backups via `pg_dump` on a CronJob, shipped to S3/GCS. Point-in-time recovery via WAL shipping (beyond k3s scope — use a managed Postgres for this).
- **Redis:** not backed up. It holds ephemeral queue state + folder cache. Loss means the sweeper re-pushes queued jobs from Postgres; folder cache rebuilds from Drive lookups on next upload. Acceptable.
- **`TOKEN_ENCRYPTION_KEY`:** the one piece of irreplaceable infra state. Back it up to a separate secure location (1Password, AWS Secrets Manager, a vault). Loss = users must re-run `oauth-setup`.
- **Data volume (`DATA_DIR`):** in-flight MIME/PDF/attachments. Loss means the scheduler re-enqueues on next poll (forward-sync advances the cursor AFTER emails are processed, so unprocessed emails are still at `historyId <= cursor` and re-sync). Accept the loss.

---

## 11. Capacity planning

Guidelines based on measured behavior.

| Scenario | Recommended spec |
|---|---|
| Personal inbox, ~100 emails/day | 1 vCPU, 2 GB RAM, 30 GB disk (Hetzner CX22, DO $12 droplet). Runs cold. |
| Personal inbox, ~1,000 emails/day | 2 vCPU, 4 GB RAM. Mostly for the Chromium renders during bursts. |
| Multi-user, 10 users × 100 emails/day | 2 vCPU, 4 GB RAM + managed Postgres. PVC for workers must be RWX. |
| Multi-user, 100+ users | Move Postgres + Redis to managed services (RDS + ElastiCache or equivalent). Scale render-worker to 3–5 replicas. Replace `/data` with object storage. |

Render-worker is always the first bottleneck. Chromium's memory footprint per render (100–300 MB) dominates the box's RAM during bursts.
