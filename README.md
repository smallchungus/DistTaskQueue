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

See [docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md](docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md) for the full design.

Three-stage pipeline: `fetch` → `render` → `upload`. Each stage is an independently autoscaled Kubernetes Deployment with its own Redis queue. Postgres holds pipeline state, status history, OAuth tokens, and idempotency keys. A sweeper requeues jobs from workers that go silent for >30 s.

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
