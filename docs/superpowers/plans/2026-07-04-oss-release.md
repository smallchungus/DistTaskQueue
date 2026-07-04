# OSS Release Implementation Plan

**Goal:** Make DistTaskQueue publishable as self-hosted open source: a stranger with a Google account succeeds in ~10 minutes via docker compose, can back-fill their existing inbox, and can pin a versioned release.

**Positioning (decided):** self-hosted only. Every operator brings their own Google OAuth client for their own data, so Google's restricted-scope verification never applies to us and no stranger's email ever touches our infrastructure. Multi-tenant SaaS is explicitly out of scope for this plan.

**Approach notes (karpathy/ponytail pass):**
- The image already contains all five binaries (`/api /worker /sweeper /scheduler /oauth-setup`, distroless) — the compose quickstart is wiring, not building.
- Historical backfill reuses the existing `HasJobForMessage` idempotency loop verbatim; the new work is (a) paginated Gmail listing and (b) queue-depth backpressure so a 50k-message inbox doesn't flood Redis. It ships as a one-shot `cmd/backfill` command, NOT a change to the scheduler — forward-sync stays the steady state.
- The design spec currently rejects backfill ("would effectively DoS the Gmail API on our own quota") — that reasoning was about automatic backfill on first sync. Spec gets amended first: operator-invoked, paged, backpressured backfill is a different animal.
- Releases: plain buildx workflow on git tags, no goreleaser. arm64 included — half of r/selfhosted runs on a Pi or an ARM VPS.
- Launch-post drafts (HN, r/selfhosted) stay out of the repo; they're chat deliverables at launch time.

## Task 1 — community/housekeeping files

Files: `LICENSE` (new), `SECURITY.md` (new), `.env.example` (new — currently missing despite CLAUDE.md mandating it), `README.md` (license section + quickstart pointer).

1. `LICENSE`: Apache-2.0, copyright 2026 Will Chen.
2. `SECURITY.md`: supported versions (latest release), private reporting via GitHub security advisories, 90-day disclosure, plus the standing notes: tokens AES-GCM at rest, content files NOT encrypted at rest, single-operator threat model.
3. `.env.example`: every var from OPERATIONS §2 with placeholder values and one-line comments — `DATABASE_URL`, `REDIS_URL`, `API_ADDR`, `GOOGLE_OAUTH_CLIENT_ID`, `GOOGLE_OAUTH_CLIENT_SECRET`, `TOKEN_ENCRYPTION_KEY`, `DATA_DIR`, `GOTENBERG_URL`, `DRIVE_ROOT_FOLDER_ID`, `DRIVE_ROOT_PATH`, scheduler/backfill/sweeper/worker tunables.
4. README: replace `License: TBD` with Apache-2.0; verify no committed local build artifacts (`bin/`, root `oauth-setup`/`scheduler`/`sweeper` binaries) are git-tracked; extend `.gitignore` if needed.
5. Verify: `git ls-files | grep -E 'bin/|^(oauth-setup|scheduler|sweeper)$'` returns nothing; `make lint` clean. Commit `chore: add Apache-2.0 license, SECURITY.md, .env.example`.

## Task 2 — one-command self-host (docker compose)

Files: `docker-compose.yaml` (extend), `docs/SELF-HOSTING.md` (new), `README.md` (quickstart section rewrite).

1. Extend compose with the missing services, all `image: ghcr.io/smallchungus/disttaskqueue-api:latest` (with a commented `build:` block for dev):
   - `worker-fetch`, `worker-render`, `worker-upload`: `entrypoint: ["/worker"]`, `command: ["--stage=<s>"]`, shared named volume `dtq-data:/data`, `env_file: .env`, depends_on postgres/redis healthy (+ gotenberg for render).
   - `scheduler`, `sweeper`: same pattern, no data volume for sweeper.
   - `oauth-setup`: `profiles: ["setup"]`, `ports: ["8888:8888"]`, `entrypoint: ["/oauth-setup"]` — run once via `docker compose run --service-ports oauth-setup --email=you@example.com`.
   - Existing api/postgres/redis/gotenberg services keep working; api gains `env_file: .env`.
2. `docs/SELF-HOSTING.md` — the 10-minute path, written for someone who has never seen the repo:
   1. Google Cloud: create project, enable Gmail + Drive APIs, OAuth consent screen (scopes `gmail.readonly` + `drive.file`, add self as test user, note about publishing to production so tokens survive 7 days), create Desktop OAuth client with `http://localhost:8888/callback`.
   2. `cp .env.example .env`, fill client id/secret, `openssl rand -base64 32` for the token key.
   3. `docker compose up -d`, then the oauth-setup run, then "send yourself an email, check Drive in ~2 minutes."
   4. Troubleshooting table: invalid_grant (Testing-mode expiry), Drive folder empty (DRIVE_ROOT_PATH), port collisions.
3. README quickstart section links to SELF-HOSTING.md as the primary path; k8s/KEDA stays documented as the "production-grade" option in OPERATIONS.
4. Verify end-to-end on this machine with a scratch `.env`: `docker compose up -d` → all services healthy → `curl localhost:8080/healthz` → oauth-setup run against my real account → email lands in Drive. Paste outputs. Commit `feat: full self-host docker compose stack + guide`.

## Task 3 — historical backfill (the feature; TDD)

Files: spec §4 + §"Forward-sync only" amendment; `internal/gmail/client.go` (paging method); `cmd/backfill/main.go` (new); `internal/backfill/full.go` + integration test (new); `docs/SELF-HOSTING.md` (section); compose `profiles: ["backfill"]` service.

1. **Spec first**: amend the forward-sync-only rationale — steady state remains forward-sync; add "operator-invoked full backfill: paginated `messages.list`, enqueue-with-idempotency per page, pause while `queue:fetch` depth exceeds a cap; quota math: messages.list = 5 units/page(100 ids), fetch raw = 5 units/msg against 250 units/s/user — the pipeline's render stage is the real throttle."
2. **Gmail client**: add `ListAllPages(ctx, since, before time.Time, page func(ids []string) error) error` — `users.messages.list` with `q:"after:X before:Y"` (zero times omitted), `pageToken` loop, call `page` per page. Integration test with the existing `httptest` fake pattern: two paginated responses, assert both pages delivered in order.
3. **Full-backfill runner** (`internal/backfill/full.go`): `RunFull(ctx, cfg FullConfig) (enqueued, skipped int, err error)` — for the named user (by email), page through `ListAllPages`, per ID reuse the exact `HasJobForMessage`→`EnqueueJob`→`Push` sequence from `runUser`, and before each page block while `Queue.Depth("fetch") > cfg.MaxQueueDepth` (default 500, poll every 5 s, log progress `backfill: page N, enqueued X, skipped Y, waiting on queue depth D`). Integration tests: (a) enqueues across pages; (b) second run skips everything (idempotent); (c) backpressure blocks until depth drains (seed queue above cap, drain in test, assert resume).
4. **`cmd/backfill`**: flags `--email` (required), `--since`, `--before` (optional YYYY-MM-DD), `--max-queue` (default 500); env `DATABASE_URL`, `REDIS_URL`, `GOOGLE_OAUTH_*`, `TOKEN_ENCRYPTION_KEY`; fail-fast on missing env per project rules; prints final `enqueued=X skipped=Y`. Add `/backfill` to the Dockerfile binary list and a compose `profiles: ["backfill"]` service.
5. Docs: SELF-HOSTING "Back up your existing inbox" — one command, expectation-setting (renders are the bottleneck; ~1–2 emails/s/worker; KEDA or compose `--scale worker-render=3` to go faster), safe to interrupt and re-run (idempotent).
6. Verify: full integration suite green under `-race`; run a real bounded backfill against my own account (`--since` one month back), paste `enqueued/skipped` and Drive result. Commits: `docs: spec operator-invoked historical backfill`, `feat(gmail): paginated full-inbox listing`, `feat(backfill): one-shot historical backfill command`.

## Task 4 — versioned releases

Files: `.github/workflows/release.yml` (new), `README.md`/`docs/SELF-HOSTING.md` (pin note), `deploy/k8s/*.yaml` unchanged (`:latest` stays for my cluster; self-hosters are told to pin).

1. Workflow on `push: tags: ["v*"]`: checkout → buildx → login ghcr (GITHUB_TOKEN, packages:write) → build-push `--platforms linux/amd64,linux/arm64` with tags `:vX.Y.Z` and `:latest`, `VERSION`/`COMMIT` build args from the tag/sha → `gh release create` with `--generate-notes`.
2. Docs: SELF-HOSTING compose examples reference `disttaskqueue-api:v0.1.0`-style pins with a "releases page" link.
3. Verify: push tag `v0.1.0-rc1` on a throwaway... no — tags are cheap but public; verify with the real `v0.1.0` after Tasks 1–3 merge: watch the run, `docker manifest inspect ghcr.io/smallchungus/disttaskqueue-api:v0.1.0` shows both architectures. Commit `ci: multi-arch release images on version tags`.

## Task 5 — launch checklist (mostly not code)

1. Repo polish: description + topics on GitHub (`gmail-backup`, `self-hosted`, `golang`, `task-queue`, `kubernetes`), social preview image (the flood chart works).
2. Chat deliverables at launch time (NOT committed): r/selfhosted post (lead with what it does + 10-minute claim + demo URL), HN Show HN (lead with the war stories + hand-rolled queue angle), both linking the blog page.
3. Pre-launch sanity: fresh-clone quickstart run on a machine without Go, README top screenshot/gif of the dashboard (optional, nice).

Execution order: 1 → 2 → 3 → 4 (each independently shippable; 3 is the long pole at roughly a day; 1+2 together are an afternoon). Task 5 happens when you actually launch.
