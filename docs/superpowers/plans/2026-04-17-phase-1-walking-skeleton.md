# Phase 1 — Walking Skeleton Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up the project's repo, build/test/CI tooling, local dev environment, and Kubernetes manifests so every subsequent feature lands on a tested, deployable foundation. No queue logic yet — just an HTTP API with a health check.

**Architecture:** A single Go binary (`cmd/api`) exposing `/healthz` and `/version`. Local dev via `docker compose up` (Postgres, Redis, API). CI via GitHub Actions runs lint, unit tests, integration tests (testcontainers-go spinning real Postgres + Redis), and a Docker image build. K8s manifests authored under `deploy/k8s/` and validated with `kubectl apply --dry-run=client`. Hetzner box provisioning + actual `kubectl apply` deferred to Phase 1.5.

**Tech Stack:** Go 1.24, `chi` router, `pgx/v5`, `go-redis/v9`, `slog`, `golangci-lint`, `testcontainers-go`, Docker, GitHub Actions, k3s manifests (vanilla Kubernetes YAML).

**Module name assumption:** `github.com/smallchungus/disttaskqueue`. If your GitHub username differs, do a project-wide find/replace on the module path before starting Section A.

**Spec reference:** `docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md`

---

## Section A — Repo bootstrap

### Task A1: Initialize Go module

**Files:**
- Create: `go.mod`

- [ ] **Step 1: Initialize the module**

```bash
cd /Users/willchen/Development/DistTaskQueue
go mod init github.com/smallchungus/disttaskqueue
```

- [ ] **Step 2: Pin Go version**

Edit `go.mod` so the directive line reads exactly:

```
go 1.24
```

- [ ] **Step 3: Verify**

```bash
cat go.mod
```

Expected output (exact):

```
module github.com/smallchungus/disttaskqueue

go 1.24
```

### Task A2: Create base directory structure

**Files:**
- Create: `cmd/api/.gitkeep`
- Create: `internal/.gitkeep`
- Create: `deploy/k8s/.gitkeep`
- Create: `.github/workflows/.gitkeep`
- Create: `scripts/.gitkeep`

- [ ] **Step 1: Create directories**

```bash
mkdir -p cmd/api internal/api internal/testutil deploy/k8s .github/workflows scripts
touch cmd/api/.gitkeep internal/.gitkeep deploy/k8s/.gitkeep .github/workflows/.gitkeep scripts/.gitkeep
```

- [ ] **Step 2: Verify**

```bash
find . -type d -not -path './.git*' -not -path './.superpowers*' -not -path './docs*' | sort
```

Expected output includes (in any order):

```
.
./.github
./.github/workflows
./cmd
./cmd/api
./deploy
./deploy/k8s
./internal
./internal/api
./internal/testutil
./scripts
```

### Task A3: Add golangci-lint config

**Files:**
- Create: `.golangci.yml`

- [ ] **Step 1: Write config**

Create `.golangci.yml` with:

```yaml
version: "2"
run:
  timeout: 3m
linters:
  default: none
  enable:
    - errcheck
    - govet
    - ineffassign
    - staticcheck
    - unused
    - gosec
    - misspell
formatters:
  enable:
    - gofmt
    - goimports
```

- [ ] **Step 2: Install golangci-lint locally (one-time)**

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```

- [ ] **Step 3: Verify lint runs (will be a no-op since no Go files yet)**

```bash
golangci-lint run ./...
```

Expected: exits 0, no output.

### Task A4: Create Makefile

**Files:**
- Create: `Makefile`

- [ ] **Step 1: Write Makefile**

```makefile
.PHONY: help test test-unit test-integration lint build run docker docker-up docker-down k8s-validate

help:
	@grep -E '^[a-zA-Z_-]+:.*?## ' Makefile | awk 'BEGIN{FS=":.*?## "}{printf "  %-20s %s\n", $$1, $$2}'

test: test-unit test-integration ## Run all tests

test-unit: ## Run unit tests with race detector
	go test -race -count=1 ./...

test-integration: ## Run integration tests (requires Docker)
	go test -race -count=1 -tags=integration ./...

lint: ## Run golangci-lint
	golangci-lint run ./...

build: ## Build the api binary
	CGO_ENABLED=0 go build -o bin/api ./cmd/api

run: ## Run the api binary locally
	go run ./cmd/api

docker: ## Build the api Docker image
	docker build -t disttaskqueue/api:dev .

docker-up: ## Start local dev stack (api, postgres, redis)
	docker compose up --build

docker-down: ## Stop local dev stack
	docker compose down -v

k8s-validate: ## Validate k8s manifests without applying
	kubectl apply --dry-run=client -f deploy/k8s/
```

- [ ] **Step 2: Verify**

```bash
make help
```

Expected: lists the targets above.

### Task A5: Initial commit

- [ ] **Step 1: Stage and commit foundation files**

```bash
git add .gitignore CLAUDE.md docs/ go.mod .golangci.yml Makefile cmd/ internal/ deploy/ .github/ scripts/
git commit -m "ci: bootstrap repo with Go module, lint config, Makefile, and design docs"
```

- [ ] **Step 2: Verify clean tree**

```bash
git status
```

Expected: `nothing to commit, working tree clean`.

---

## Section B — HTTP server with health check (TDD)

### Task B1: Write failing test for healthz handler

**Files:**
- Test: `internal/api/handlers_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/api/handlers_test.go`:

```go
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthz_ReturnsOK(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	healthz(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); body != "ok" {
		t.Fatalf("body: got %q, want %q", body, "ok")
	}
}
```

- [ ] **Step 2: Run test, expect failure**

```bash
go test ./internal/api/...
```

Expected: build error — `undefined: healthz`.

### Task B2: Implement healthz handler

**Files:**
- Create: `internal/api/handlers.go`

- [ ] **Step 1: Write the handler**

Create `internal/api/handlers.go`:

```go
package api

import "net/http"

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
```

- [ ] **Step 2: Run test, expect pass**

```bash
go test ./internal/api/...
```

Expected: `ok  github.com/smallchungus/disttaskqueue/internal/api  <duration>`.

- [ ] **Step 3: Commit**

```bash
git add internal/api/
git commit -m "api: add healthz handler"
```

### Task B3: Write failing test for version handler

**Files:**
- Test: `internal/api/handlers_test.go`

- [ ] **Step 1: Append the failing test**

Append to `internal/api/handlers_test.go`:

```go
func TestVersion_ReturnsConfiguredVersion(t *testing.T) {
	h := versionHandler("v0.1.0", "abc123")

	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	want := `{"version":"v0.1.0","commit":"abc123"}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %q, want %q", got, want)
	}
}
```

You'll also need to add `"encoding/json"` to imports — but resist; the handler can build the body itself, no need to import json in the test.

- [ ] **Step 2: Run, expect failure**

```bash
go test ./internal/api/...
```

Expected: build error — `undefined: versionHandler`.

### Task B4: Implement version handler

**Files:**
- Modify: `internal/api/handlers.go`

- [ ] **Step 1: Append handler**

Append to `internal/api/handlers.go`:

```go
import (
	"encoding/json"
	"net/http"
)

type versionPayload struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

func versionHandler(version, commit string) http.HandlerFunc {
	body, _ := json.Marshal(versionPayload{Version: version, Commit: commit})
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}
```

Adjust the existing import block in `handlers.go` to consolidate (single `import (...)` block at the top).

- [ ] **Step 2: Run, expect pass**

```bash
go test ./internal/api/...
```

Expected: both tests pass.

- [ ] **Step 3: Commit**

```bash
git add internal/api/
git commit -m "api: add version handler"
```

### Task B5: Write failing test for router wiring

**Files:**
- Test: `internal/api/server_test.go`

- [ ] **Step 1: Write the test**

Create `internal/api/server_test.go`:

```go
package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouter_WiresHealthzAndVersion(t *testing.T) {
	r := NewRouter(Config{Version: "v0.1.0", Commit: "abc123"})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	cases := []struct {
		path     string
		wantBody string
	}{
		{"/healthz", "ok"},
		{"/version", `{"version":"v0.1.0","commit":"abc123"}`},
	}

	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + c.path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = resp.Body.Close() })

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status: got %d", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if string(body) != c.wantBody {
				t.Fatalf("body: got %q, want %q", string(body), c.wantBody)
			}
		})
	}
}
```

- [ ] **Step 2: Run, expect failure**

```bash
go test ./internal/api/...
```

Expected: build error — `undefined: NewRouter`, `undefined: Config`.

### Task B6: Implement Router

**Files:**
- Create: `internal/api/server.go`

- [ ] **Step 1: Add chi dependency**

```bash
go get github.com/go-chi/chi/v5
```

- [ ] **Step 2: Write the router**

Create `internal/api/server.go`:

```go
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

type Config struct {
	Version string
	Commit  string
}

func NewRouter(cfg Config) http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", healthz)
	r.Get("/version", versionHandler(cfg.Version, cfg.Commit))
	return r
}
```

- [ ] **Step 3: Run, expect pass**

```bash
go test ./internal/api/...
```

Expected: 3 tests pass.

- [ ] **Step 4: Commit**

```bash
git add internal/api/ go.mod go.sum
git commit -m "api: wire healthz and version through chi router"
```

### Task B7: Wire main.go

**Files:**
- Create: `cmd/api/main.go`

- [ ] **Step 1: Delete the .gitkeep**

```bash
rm cmd/api/.gitkeep
```

- [ ] **Step 2: Write main**

Create `cmd/api/main.go`:

```go
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/smallchungus/disttaskqueue/internal/api"
)

var (
	version = "dev"
	commit  = "none"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	addr := envOr("API_ADDR", ":8080")

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewRouter(api.Config{Version: version, Commit: commit}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("api listening", "addr", addr, "version", version, "commit", commit)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown failed", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 3: Verify build**

```bash
make build
```

Expected: `bin/api` is produced; no errors.

- [ ] **Step 4: Verify it runs and serves**

In one terminal:

```bash
make run
```

In another:

```bash
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/healthz
curl -s http://localhost:8080/version
```

Expected: `200`, then `{"version":"dev","commit":"none"}`. Stop the server with Ctrl+C.

- [ ] **Step 5: Commit**

```bash
git add cmd/api/
git commit -m "api: add main with graceful shutdown and slog json output"
```

---

## Section B-bis — Git hooks (local dev safety net)

Local pre-commit and pre-push hooks. Pre-commit is fast (gofmt + vet + short unit tests) and runs on every commit. Pre-push runs lint + full unit tests. Integration tests stay in CI only (they need Docker and are slow). Hooks are opt-in — devs run `make install-hooks` once after clone.

### Task BB1: Hook scripts and installer

**Files:**
- Create: `scripts/hooks/pre-commit`
- Create: `scripts/hooks/pre-push`
- Create: `scripts/install-hooks.sh`
- Modify: `Makefile` (add `install-hooks` target)
- Modify: `README.md` later in Section H references this — no change here

- [ ] **Step 1: Write pre-commit hook**

Create `scripts/hooks/pre-commit`:

```bash
#!/usr/bin/env bash
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

unformatted=$(gofmt -l . 2>/dev/null | grep -v '^vendor/' || true)
if [ -n "$unformatted" ]; then
  echo "pre-commit: gofmt issues in:" >&2
  echo "$unformatted" >&2
  echo "run: gofmt -w ." >&2
  exit 1
fi

go vet ./...
go test -short -count=1 ./...
```

- [ ] **Step 2: Write pre-push hook**

Create `scripts/hooks/pre-push`:

```bash
#!/usr/bin/env bash
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

if command -v golangci-lint >/dev/null 2>&1; then
  golangci-lint run ./...
else
  echo "pre-push: golangci-lint not installed, skipping lint" >&2
fi

go test -race -count=1 ./...
```

- [ ] **Step 3: Write installer**

Create `scripts/install-hooks.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

hooks_dir="$(git rev-parse --git-path hooks)"
mkdir -p "$hooks_dir"

for hook in pre-commit pre-push; do
  src="scripts/hooks/$hook"
  dst="$hooks_dir/$hook"
  ln -sf "$(pwd)/$src" "$dst"
  chmod +x "$src"
  echo "installed: $dst -> $src"
done
```

- [ ] **Step 4: Add Makefile target**

Append to `Makefile` (under `.PHONY` line — add `install-hooks` to the list — and add a new target):

`.PHONY` line becomes:

```
.PHONY: help test test-unit test-integration lint build run docker docker-up docker-down k8s-validate install-hooks
```

Add target (place near `lint` for grouping):

```makefile
install-hooks: ## Install git pre-commit and pre-push hooks
	./scripts/install-hooks.sh
```

- [ ] **Step 5: Make the scripts executable and install**

```bash
chmod +x scripts/hooks/pre-commit scripts/hooks/pre-push scripts/install-hooks.sh
make install-hooks
```

Expected: prints two `installed: ...` lines.

- [ ] **Step 6: Verify pre-commit fires by attempting a no-op commit**

```bash
git commit --allow-empty -m "test: verify pre-commit hook fires" --dry-run
```

Actually, `--dry-run` skips hooks. Real verification: stage one trivial change and try a commit, expecting the hook to run. Skip to next step instead.

- [ ] **Step 7: Stage hook files and commit (the hook will run on this very commit, validating itself)**

```bash
git add scripts/ Makefile
git commit -m "ci: add pre-commit and pre-push git hooks with installer"
```

Expected: commit succeeds. The pre-commit hook ran gofmt + vet + short tests against the current state, all clean.

- [ ] **Step 8: Verify hook is installed**

```bash
ls -la "$(git rev-parse --git-path hooks)"/pre-commit "$(git rev-parse --git-path hooks)"/pre-push
```

Expected: both shown as symlinks pointing into `scripts/hooks/`.

---

## Section C — testcontainers helper + integration test scaffold

### Task C1: Add dependencies

- [ ] **Step 1: Get the modules**

```bash
go get github.com/testcontainers/testcontainers-go
go get github.com/testcontainers/testcontainers-go/modules/postgres
go get github.com/testcontainers/testcontainers-go/modules/redis
go get github.com/jackc/pgx/v5
go get github.com/redis/go-redis/v9
```

- [ ] **Step 2: Tidy**

```bash
go mod tidy
```

### Task C2: Write Postgres container helper

**Files:**
- Create: `internal/testutil/postgres.go`

- [ ] **Step 1: Write helper**

Create `internal/testutil/postgres.go`:

```go
//go:build integration

package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func StartPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	c, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("dtq_test"),
		postgres.WithUsername("dtq"),
		postgres.WithPassword("dtq"),
		postgres.BasicWaitStrategies(),
		postgres.WithSQLDriver("pgx"),
	)
	if err != nil {
		t.Fatalf("postgres start: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Terminate(context.Background())
	})

	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.MaxConnIdleTime = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool
}
```

### Task C3: Write Redis container helper

**Files:**
- Create: `internal/testutil/redis.go`

- [ ] **Step 1: Write helper**

Create `internal/testutil/redis.go`:

```go
//go:build integration

package testutil

import (
	"context"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go/modules/redis"
)

func StartRedis(t *testing.T) *goredis.Client {
	t.Helper()
	ctx := context.Background()

	c, err := redis.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatalf("redis start: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Terminate(context.Background())
	})

	uri, err := c.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("uri: %v", err)
	}
	opts, err := goredis.ParseURL(uri)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cli := goredis.NewClient(opts)
	t.Cleanup(func() { _ = cli.Close() })

	if err := cli.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return cli
}
```

### Task C4: Write a smoke integration test

**Files:**
- Test: `internal/testutil/smoke_integration_test.go`

- [ ] **Step 1: Write the test**

Create `internal/testutil/smoke_integration_test.go`:

```go
//go:build integration

package testutil

import (
	"context"
	"testing"
)

func TestStartPostgres_AcceptsQueries(t *testing.T) {
	pool := StartPostgres(t)
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT 1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("got %d, want 1", n)
	}
}

func TestStartRedis_AcceptsCommands(t *testing.T) {
	cli := StartRedis(t)
	if err := cli.Set(context.Background(), "k", "v", 0).Err(); err != nil {
		t.Fatal(err)
	}
	got, err := cli.Get(context.Background(), "k").Result()
	if err != nil {
		t.Fatal(err)
	}
	if got != "v" {
		t.Fatalf("got %q, want %q", got, "v")
	}
}
```

- [ ] **Step 2: Run integration tests, expect pass**

```bash
make test-integration
```

Expected: both tests pass. First run will pull `postgres:16-alpine` and `redis:7-alpine` images. Allow 1-3 minutes.

- [ ] **Step 3: Verify unit tests still pass**

```bash
make test-unit
```

Expected: all green.

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum internal/testutil/
git commit -m "test: add testcontainers helpers and smoke integration tests"
```

---

## Section D — Dockerfile

### Task D1: Multi-stage Dockerfile

**Files:**
- Create: `Dockerfile`
- Create: `.dockerignore`

- [ ] **Step 1: Write Dockerfile**

Create `Dockerfile`:

```dockerfile
# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/api"]
```

- [ ] **Step 2: Write .dockerignore**

Create `.dockerignore`:

```
.git
.github
.gitignore
.golangci.yml
.superpowers
.vscode
.idea
bin
coverage.out
coverage.html
deploy
docs
README.md
Makefile
*.md
docker-compose.yaml
```

- [ ] **Step 3: Build the image**

```bash
docker build --build-arg VERSION=dev --build-arg COMMIT=$(git rev-parse --short HEAD) -t disttaskqueue/api:dev .
```

Expected: build succeeds; final image tagged.

- [ ] **Step 4: Run the image and probe**

```bash
docker run --rm -d --name dtq-api-test -p 8080:8080 disttaskqueue/api:dev
sleep 1
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/healthz
curl -s http://localhost:8080/version
docker stop dtq-api-test
```

Expected: `200`, then `{"version":"dev","commit":"<short sha>"}`.

- [ ] **Step 5: Commit**

```bash
git add Dockerfile .dockerignore
git commit -m "ci: add multi-stage Dockerfile with distroless runtime"
```

---

## Section E — docker-compose for local dev

### Task E1: Compose file

**Files:**
- Create: `docker-compose.yaml`
- Create: `.env.example`

- [ ] **Step 1: Write compose file**

Create `docker-compose.yaml`:

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: dtq
      POSTGRES_USER: dtq
      POSTGRES_PASSWORD: dtq
    ports:
      - "5432:5432"
    volumes:
      - pg-data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U dtq"]
      interval: 2s
      timeout: 2s
      retries: 10

  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"
    volumes:
      - redis-data:/data
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 2s
      timeout: 2s
      retries: 10

  api:
    build:
      context: .
      args:
        VERSION: dev
        COMMIT: local
    environment:
      API_ADDR: ":8080"
      DATABASE_URL: "postgres://dtq:dtq@postgres:5432/dtq?sslmode=disable"
      REDIS_URL: "redis://redis:6379/0"
    ports:
      - "8080:8080"
    depends_on:
      postgres: { condition: service_healthy }
      redis: { condition: service_healthy }

volumes:
  pg-data:
  redis-data:
```

- [ ] **Step 2: Write .env.example**

Create `.env.example`:

```
# Copy to .env for local overrides; .env is gitignored.
API_ADDR=:8080
DATABASE_URL=postgres://dtq:dtq@localhost:5432/dtq?sslmode=disable
REDIS_URL=redis://localhost:6379/0
```

- [ ] **Step 3: Verify compose stack**

```bash
make docker-up
```

In another terminal:

```bash
sleep 5
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/healthz
```

Expected: `200`. Then in the original terminal, Ctrl+C and:

```bash
make docker-down
```

- [ ] **Step 4: Commit**

```bash
git add docker-compose.yaml .env.example
git commit -m "ci: add docker-compose stack for local dev"
```

---

## Section F — GitHub Actions CI

### Task F1: CI workflow

**Files:**
- Create: `.github/workflows/ci.yml`

- [ ] **Step 1: Write workflow**

Create `.github/workflows/ci.yml`:

```yaml
name: ci

on:
  push:
    branches: [main]
  pull_request:

jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: "1.25" }
      - uses: golangci/golangci-lint-action@v6
        with: { version: latest }

  unit:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: "1.25" }
      - run: go test -race -count=1 ./...

  integration:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: "1.25" }
      - run: go test -race -count=1 -tags=integration ./...

  image:
    runs-on: ubuntu-latest
    needs: [lint, unit]
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-buildx-action@v3
      - name: build image
        uses: docker/build-push-action@v6
        with:
          context: .
          push: false
          tags: disttaskqueue/api:ci
          build-args: |
            VERSION=ci-${{ github.sha }}
            COMMIT=${{ github.sha }}
```

- [ ] **Step 2: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: add GitHub Actions workflow for lint, unit, integration, image"
```

(Push happens later, after the GitHub repo exists.)

---

## Section G — Kubernetes manifests (authored, dry-run-validated)

### Task G1: Namespace + ConfigMap + Secret example

**Files:**
- Create: `deploy/k8s/00-namespace.yaml`
- Create: `deploy/k8s/01-config.yaml`
- Create: `deploy/k8s-examples/secrets.yaml`

The real Secret is created out-of-band via `kubectl create secret`. The example lives in `deploy/k8s-examples/` (separate directory) so `make k8s-validate` and `kubectl apply -f deploy/k8s/` don't pick it up by accident.

- [ ] **Step 1: Delete the .gitkeep and create example dir**

```bash
rm deploy/k8s/.gitkeep
mkdir -p deploy/k8s-examples
```

- [ ] **Step 2: Write namespace**

Create `deploy/k8s/00-namespace.yaml`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: disttaskqueue
```

- [ ] **Step 3: Write configmap**

Create `deploy/k8s/01-config.yaml`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: dtq-config
  namespace: disttaskqueue
data:
  API_ADDR: ":8080"
  DATABASE_URL: "postgres://dtq:dtq@postgres:5432/dtq?sslmode=disable"
  REDIS_URL: "redis://redis:6379/0"
```

- [ ] **Step 4: Write secret example (NOT in deploy/k8s/ — won't be applied)**

Create `deploy/k8s-examples/secrets.yaml`:

```yaml
# Do NOT commit a real Secret. Create it on the cluster with:
#
#   kubectl -n disttaskqueue create secret generic dtq-secrets \
#     --from-literal=POSTGRES_PASSWORD=...changeme... \
#     --from-literal=TOKEN_ENCRYPTION_KEY=...32-byte-base64...
#
# This file shows the shape only.
apiVersion: v1
kind: Secret
metadata:
  name: dtq-secrets
  namespace: disttaskqueue
type: Opaque
stringData:
  POSTGRES_PASSWORD: "REPLACE_ME"
  TOKEN_ENCRYPTION_KEY: "REPLACE_ME_BASE64_32_BYTES"
```

### Task G2: Postgres StatefulSet

**Files:**
- Create: `deploy/k8s/10-postgres.yaml`

- [ ] **Step 1: Write Postgres**

Create `deploy/k8s/10-postgres.yaml`:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: postgres
  namespace: disttaskqueue
spec:
  selector: { app: postgres }
  ports:
    - port: 5432
      targetPort: 5432
  clusterIP: None
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: postgres
  namespace: disttaskqueue
spec:
  serviceName: postgres
  replicas: 1
  selector:
    matchLabels: { app: postgres }
  template:
    metadata:
      labels: { app: postgres }
    spec:
      containers:
        - name: postgres
          image: postgres:16-alpine
          ports: [{ containerPort: 5432 }]
          env:
            - name: POSTGRES_DB
              value: dtq
            - name: POSTGRES_USER
              value: dtq
            - name: POSTGRES_PASSWORD
              valueFrom:
                secretKeyRef: { name: dtq-secrets, key: POSTGRES_PASSWORD }
          volumeMounts:
            - name: data
              mountPath: /var/lib/postgresql/data
          readinessProbe:
            exec: { command: ["pg_isready", "-U", "dtq"] }
            initialDelaySeconds: 5
            periodSeconds: 5
  volumeClaimTemplates:
    - metadata: { name: data }
      spec:
        accessModes: [ReadWriteOnce]
        resources: { requests: { storage: 5Gi } }
```

### Task G3: Redis StatefulSet

**Files:**
- Create: `deploy/k8s/11-redis.yaml`

- [ ] **Step 1: Write Redis**

Create `deploy/k8s/11-redis.yaml`:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: redis
  namespace: disttaskqueue
spec:
  selector: { app: redis }
  ports:
    - port: 6379
      targetPort: 6379
  clusterIP: None
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: redis
  namespace: disttaskqueue
spec:
  serviceName: redis
  replicas: 1
  selector:
    matchLabels: { app: redis }
  template:
    metadata:
      labels: { app: redis }
    spec:
      containers:
        - name: redis
          image: redis:7-alpine
          args: ["--appendonly", "yes"]
          ports: [{ containerPort: 6379 }]
          volumeMounts:
            - name: data
              mountPath: /data
          readinessProbe:
            exec: { command: ["redis-cli", "ping"] }
            initialDelaySeconds: 2
            periodSeconds: 5
  volumeClaimTemplates:
    - metadata: { name: data }
      spec:
        accessModes: [ReadWriteOnce]
        resources: { requests: { storage: 1Gi } }
```

### Task G4: API Deployment + Service

**Files:**
- Create: `deploy/k8s/20-api.yaml`

- [ ] **Step 1: Write API deployment**

Create `deploy/k8s/20-api.yaml`:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: api
  namespace: disttaskqueue
spec:
  selector: { app: api }
  ports:
    - port: 80
      targetPort: 8080
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: disttaskqueue
spec:
  replicas: 1
  selector:
    matchLabels: { app: api }
  template:
    metadata:
      labels: { app: api }
    spec:
      containers:
        - name: api
          image: ghcr.io/smallchungus/disttaskqueue-api:latest
          imagePullPolicy: Always
          ports: [{ containerPort: 8080 }]
          envFrom:
            - configMapRef: { name: dtq-config }
          readinessProbe:
            httpGet: { path: /healthz, port: 8080 }
            initialDelaySeconds: 2
            periodSeconds: 5
          livenessProbe:
            httpGet: { path: /healthz, port: 8080 }
            initialDelaySeconds: 5
            periodSeconds: 10
          resources:
            requests: { cpu: 50m, memory: 64Mi }
            limits:   { cpu: 500m, memory: 256Mi }
```

### Task G5: Validate manifests

- [ ] **Step 1: Dry-run apply**

```bash
make k8s-validate
```

Expected output (each line a `(dry run)` confirmation; example secret is NOT applied because it lives in `deploy/k8s-examples/`):

```
namespace/disttaskqueue created (dry run)
configmap/dtq-config created (dry run)
service/postgres created (dry run)
statefulset.apps/postgres created (dry run)
service/redis created (dry run)
statefulset.apps/redis created (dry run)
service/api created (dry run)
deployment.apps/api created (dry run)
```

If `kubectl` is not installed, install it first (`brew install kubectl` on macOS).

- [ ] **Step 2: Commit**

```bash
git add deploy/k8s/ deploy/k8s-examples/
git commit -m "k8s: add manifests for namespace, config, postgres, redis, api"
```

---

## Section H — README

### Task H1: Write README

**Files:**
- Create: `README.md`

- [ ] **Step 1: Write README**

Create `README.md`:

````markdown
# DistTaskQueue

A distributed task queue in Go (Redis + Postgres + k8s) that powers a continuous Gmail → PDF → Google Drive sync. A hand-rolled cloudhq replacement, and a portfolio piece demonstrating multi-stage pipelines, heartbeat-based failure recovery, autoscaling, and at-least-once delivery with idempotent side effects.

## Status

Phase 1 (walking skeleton) — see `docs/superpowers/plans/` for the build sequence.

## Quickstart (local)

Prereqs: Go 1.24+, Docker.

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
make test-integration  # spins up real Postgres + Redis via testcontainers
make lint
```

## Architecture

See [docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md](docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md).

Three-stage pipeline: `fetch` → `render` → `upload`. Each stage is an independently autoscaled Kubernetes Deployment. Redis holds the queues and worker heartbeats. Postgres holds pipeline state, status history, OAuth tokens, and idempotency keys. A sweeper requeues jobs from workers that go silent for >30 s.

## Deploy

K8s manifests under `deploy/k8s/`. Validate without applying:

```bash
make k8s-validate
```

Production target is k3s on a single Hetzner CX22, fronted by Cloudflare Tunnel for TLS without exposing the box. Provisioning steps land in Phase 1.5.

## Repo layout

```
cmd/api/         # HTTP API binary
internal/api/    # router + handlers
internal/testutil/  # testcontainers helpers (integration build tag)
deploy/k8s/      # Kubernetes manifests
docs/superpowers/  # specs and implementation plans
```

## License

TBD.
````

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: add README with quickstart, layout, and architecture pointer"
```

---

## Section I — Final verification

### Task I1: Full local pipeline runs green

- [ ] **Step 1: Tidy and verify modules**

```bash
go mod tidy
git status
```

Expected: no unintended changes; `go.mod`/`go.sum` already committed.

- [ ] **Step 2: Run all checks the way CI will**

```bash
make lint
make test-unit
make test-integration
docker build --build-arg VERSION=phase1 --build-arg COMMIT=$(git rev-parse --short HEAD) -t disttaskqueue/api:phase1 .
make k8s-validate
```

Expected: every command exits 0.

- [ ] **Step 3: Tag the milestone**

```bash
git tag -a phase-1 -m "Walking skeleton: api + tests + ci + manifests"
git log --oneline
```

Expected: a clean linear history of the commits made above, with `phase-1` tag at HEAD.

---

## Out of scope for Phase 1 (explicit non-goals)

These land in later plans and should NOT be added to Phase 1:

- Any queue logic (Redis LPUSH/BLPOP, claim semantics, sweeper).
- Any worker binaries (`cmd/worker`, `cmd/scheduler`, `cmd/sweeper`).
- Postgres schema migrations beyond the empty database.
- Gmail or Drive client code.
- Dashboard HTML / SSE.
- Cloudflare Tunnel deployment manifest.
- HPA manifests, Prometheus, metrics adapter.
- GitHub Actions deploy job (requires the Hetzner box to exist).

## Phase 1.5 (separate, larger plan after Phase 1 ships)

The "go from green CI to live URL with promotion gates" plan.

- Provision Hetzner CX22, install k3s, copy kubeconfig to GitHub repo secret.
- Two k3s namespaces on the same box: `dtq-staging` and `dtq-production`.
- Two GitHub Environments: `staging` and `production`. Per-environment secrets (kubeconfig, image pull, encryption key).
- Deploy Cloudflare Tunnel as a k8s Deployment for each namespace; map two subdomains (e.g., `staging.dtq.example` and `dtq.example`).
- New workflows:
  - `deploy-staging.yml` — on push to `main`: build + push image tagged `:sha-<short>` to GHCR, `kubectl apply` to `dtq-staging`.
  - `smoke-staging.yml` — after staging deploy: curl staging URL, assert `/healthz` and `/version`. On green, write a tag `release-<sha>`.
  - `deploy-production.yml` — on tag push matching `release-*`: gated by GitHub Environment manual approval (single click), `kubectl apply` to `dtq-production`.
- PR auto-merge: enable GitHub auto-merge on the repo, set required checks to `lint`, `unit`, `integration`, `image`. Once green, GitHub merges PRs automatically.
- First public hello-world URL serving `/healthz` and `/version` from `dtq-production`, plus a separate staging URL.

## Phase 2 preview (separate plan)

Queue core in Go: Redis-backed `Queue`, Postgres-backed `Store`, heartbeat sweeper, exponential backoff, full TDD with testcontainers. Single test workload (a no-op job that sleeps for N ms) end-to-end through one stage. Earns the resume claim of "5K jobs across 4 workers in ~10 s."
