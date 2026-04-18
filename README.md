# DistTaskQueue

[![ci](https://github.com/smallchungus/DistTaskQueue/actions/workflows/ci.yml/badge.svg)](https://github.com/smallchungus/DistTaskQueue/actions/workflows/ci.yml)

A distributed task queue in Go (Redis + Postgres + Kubernetes), built to power a continuous Gmail → PDF → Google Drive sync. A hand-rolled cloudhq replacement, and a portfolio piece demonstrating multi-stage pipelines, heartbeat-based failure recovery, autoscaling on queue depth, and at-least-once delivery with idempotent side effects.

## Status

Phase 2 complete — queue core ships end-to-end. Postgres-backed durable state (`pipeline_jobs`, `job_status_history`), Redis-backed per-stage queues, worker binary with claim + heartbeat + exponential-backoff retries, sweeper with orphan revival and delayed-retry promotion. Gmail/Drive workers and live deployment ship in later phases. See `docs/superpowers/plans/`.

## Quickstart (local)

Prereqs: Go 1.25+, Docker.

```bash
make docker-up        # brings up api + postgres + redis on docker compose
curl localhost:8080/healthz
curl localhost:8080/version
make docker-down
```

Run the API directly without Docker:

```bash
make run
```

## Real Gmail → Drive setup (one-time)

If you want to actually run the Gmail-to-Drive sync (rather than just synthetic
loadtest jobs), you need to set up a Google Cloud OAuth client and bootstrap a
token. About 10 minutes of clicking.

1. **Google Cloud Console:** Create (or reuse) a project. Enable the **Gmail API**
   and **Drive API**. On the OAuth consent screen, set the scopes to
   `gmail.readonly` and `drive.file`. Add your email as a test user.

2. **OAuth client:** Create an OAuth 2.0 Client ID, type **Desktop**.
   Add `http://localhost:8888/callback` as an authorized redirect URI.
   Note the client ID and secret.

3. **Encryption key:** Generate a 32-byte random key:

   ```bash
   openssl rand -base64 32
   ```

   Save it; the same value is needed by both `oauth-setup` and the workers/scheduler.

4. **Bring up the local stack:**

   ```bash
   make docker-up
   ```

5. **Bootstrap your OAuth token:**

   ```bash
   export DATABASE_URL='postgres://dtq:dtq@localhost:5432/dtq?sslmode=disable'
   export GOOGLE_OAUTH_CLIENT_ID='...'
   export GOOGLE_OAUTH_CLIENT_SECRET='...'
   export TOKEN_ENCRYPTION_KEY='<the base64 key>'
   go run ./cmd/oauth-setup --email=you@example.com
   ```

   Open the printed URL in a browser, authorize the app, the helper exits with
   "Token saved".

6. **Run the workers + scheduler:**

   ```bash
   # in one terminal each, or via docker compose:
   ./worker --stage=fetch
   ./worker --stage=render
   ./worker --stage=upload
   ./scheduler
   ```

   On the first poll the scheduler initializes your cursor from your current
   Gmail history ID (no backfill of existing inbox). Subsequent polls pick up
   new INBOX/PRIMARY messages and push them through the fetch → render → upload
   pipeline.

7. **Where do the PDFs land?** A new folder tree at the root of your Drive:
   `<root>/YYYY/MM/DD/`. Set `DRIVE_ROOT_FOLDER_ID` to the Drive folder ID where
   you want this tree to live (or leave unset to land at the root).

## Tests

```bash
make test-unit         # fast, no Docker
make test-integration  # spins up real Postgres + Redis via testcontainers-go
make lint              # golangci-lint
```

Integration tests use real Redis and real Postgres via [testcontainers-go](https://golang.testcontainers.org/) — never mocks. Mocking the queue and storage layers is how subtle bugs (race conditions, ordering, atomicity) escape into production.

## Load testing

```bash
make loadtest
```

Spins up real Postgres + Redis containers via testcontainers-go, enqueues 5000 no-op jobs, runs 4 workers as goroutines in parallel, and asserts the whole run completes under 15 seconds. Recent runs land around 2 seconds on a dev laptop.

Each job exercises the full path: insert into `pipeline_jobs`, `LPUSH` to a Redis stage list, `BRPOP` from a worker, atomic claim via `UPDATE ... RETURNING`, heartbeat goroutine, then `MarkDone`. Workers compete at Postgres and Redis, not in Go memory, so the in-process version stresses the same atomic primitives as a multi-process deployment.

## Git hooks

Optional but recommended. Install once after clone:

```bash
make install-hooks
```

- `pre-commit` — `gofmt`, `go vet`, `go test -short ./...`. Fast.
- `pre-push` — `golangci-lint`, full unit tests with `-race`.

Integration tests stay in CI only (they need Docker and are slow).

## Architecture

Deep walkthrough: **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — why each component looks the way it does, what alternatives were considered and rejected, what correctness the system guarantees, where it scales and where it doesn't.

Operations / runbooks: **[docs/OPERATIONS.md](docs/OPERATIONS.md)** — cloud swap, scaling, OAuth bootstrap, disaster recovery.

Three-stage pipeline: `fetch` → `render` → `upload`. Each stage is an independently autoscaled Kubernetes Deployment with its own Redis queue. Postgres holds pipeline state, status history, OAuth tokens, and idempotency keys. A sweeper requeues jobs from workers that go silent for >30 s.

Prometheus metrics are exported at `GET /metrics` — queue depth per stage, job counts per status, live worker count. Ready for Grafana or HPA.

```
                              ┌──────────────┐
                              │   Browser    │
                              │  (dashboard) │
                              └──────┬───────┘
                                     │ HTTPS via Cloudflare Tunnel
                                     ▼
                              ┌──────────────┐
       ┌─────────────────────▶│  API server  │◀── demo trigger buttons
       │                      │  Go, net/http│
       │                      └──────┬───────┘
       │     ┌───────────────────────┼───────────────────────┐
       │     ▼                       ▼                       ▼
       │  ┌──────┐               ┌────────┐              ┌──────────┐
       │  │Redis │  queues +     │Postgres│  pipeline    │ Local PV │
       │  │      │  heartbeats   │        │  state, OAuth│ (volume) │
       │  └──────┘               └────────┘              └──────────┘
       │     ▲     ▲     ▲          ▲                      ▲       ▲
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
       (polls Gmail History every 5 min)
```

## Deploy

K8s manifests under `deploy/k8s/`. Validate offline:

```bash
make k8s-validate     # uses kubeconform — `brew install kubeconform`
```

Production target is k3s on a single Hetzner CX22, fronted by Cloudflare Tunnel for TLS without exposing the box. Provisioning + first deploy land in Phase 1.5.

## Repo layout

```
cmd/api/             # HTTP API binary
internal/api/        # router + handlers
internal/testutil/   # testcontainers helpers (integration build tag)
deploy/k8s/          # Kubernetes manifests (applied)
deploy/k8s-examples/ # example Secret shape (NOT applied)
scripts/hooks/       # git pre-commit and pre-push
docs/superpowers/    # specs and implementation plans
```

## Contributing

This is a solo portfolio project, but the conventions are real:

- TDD. Tests before implementation. Integration tests use real Postgres + Redis.
- Conventional commits (`feat:`, `fix:`, `chore:`, etc.) with optional component scope (`feat(api):`, `chore(k8s):`).
- One concern per PR.
- See [CLAUDE.md](CLAUDE.md) for the full project rules (the file is consumed by the AI assistant, but it's also the source of truth for human contributors).

## License

TBD.
