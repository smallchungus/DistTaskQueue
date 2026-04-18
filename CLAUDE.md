# DistTaskQueue — Project Rules

Global rules in `~/.claude/CLAUDE.md` still apply. This file adds project-specific rules and reinforces the ones that matter most here.

## Read first

- Design spec: `docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md`
- Implementation plan(s): `docs/superpowers/plans/` (created when work starts)
- Don't deviate from the spec without flagging it. If the spec is wrong, fix the spec first, then the code.

## TDD is non-negotiable

- Tests come before implementation. Red → green → refactor. Use the `superpowers:test-driven-development` skill.
- If a test is hard to write, the unit boundary is wrong. Split before adding more code.
- Integration tests use real Redis and real Postgres via `testcontainers-go`. Do NOT mock SQL drivers or `go-redis`. Mocks here have caused outages elsewhere; we're not repeating that.
- Mock only at outbound HTTP boundaries: Gmail API, Drive API, Gotenberg. Use `httptest.Server` with canned responses, not interface mocks.

## Tests: same quality bar as production code

Tests are read more than they're written. Slop in tests is worse than slop in code because it's the first thing a reviewer sees.

- **No "this test verifies that..." comments.** The test name says it. If the name doesn't, rename the test.
- **Test names: short, declarative.** `TestSweeper_RequeuesOrphanedJob`. Not `TestSweeper_ShouldEventuallyRequeueAnOrphanedJobAfterTheHeartbeatHasTimedOut`.
- **One behavior per test.** Don't pile assertions across unrelated concerns.
- **Table-driven when there are 3+ similar cases.** Otherwise plain `t.Run` subtests.
- **No `assert.True(t, x == y)`.** Use `assert.Equal` / `cmp.Diff`. Failure messages matter.
- **No setup ceremonies.** If `setupTest` is 40 lines, the unit is too big. Test helpers OK, sprawl not.
- **No leftover `t.Log` / `fmt.Println` from debugging.** Delete before committing.

## Code

- Default to no comments. The global rule applies. Project reinforcement: especially in worker loops and queue logic, prefer self-explanatory names over a comment trying to rescue a bad name.
- One short line max for any comment. No multi-paragraph docstrings on Go functions. The function signature plus a 5-word doc comment (when exported) is enough.
- **Code can be verbose where clarity demands it. Comments cannot.** If a function needs 40 lines to be correct, that's fine. If it needs 10 lines of comments to be understood, rewrite it.
- No clever one-liners. Boring, readable Go wins. If you reach for `chan struct{}{}` patterns to look smart, you're doing it wrong.
- Minimal diffs. No drive-by refactors. If you spot something unrelated, mention it in the chat or open a follow-up — don't bundle it.
- No defensive nil checks for values that internal code guarantees aren't nil. Validate at the system boundary (HTTP handler, Gmail/Drive response parsing) and trust internals.

## Config and secrets (12-factor)

- **Everything via env vars or flags.** No hardcoded endpoints, no hardcoded timeouts, no hardcoded "dev mode on". If it might ever need to differ between local dev, staging, and prod, it's a config knob. Examples already in this repo: `DATABASE_URL`, `REDIS_URL`, `GOTENBERG_URL`, `DRIVE_ROOT_FOLDER_ID`, `DATA_DIR`, `API_ADDR`, `GOOGLE_OAUTH_CLIENT_ID`, `GOOGLE_OAUTH_CLIENT_SECRET`, `TOKEN_ENCRYPTION_KEY`.
- **`.env.example` is the canonical config template.** It must list every env var the binaries read, with placeholder values. Update it whenever a new var is added.
- **`.env` is gitignored.** Never commit real values. Never. The `.gitignore` has explicit patterns for `.env`, `.env.*`, `*.pem`, `*.key`, `*.crt`, `credentials.json`, `client_secret*.json`, `service-account*.json`, `secrets/`, `*.kubeconfig`. Add new patterns when new secret shapes appear.
- **Secrets in prod come from the platform's secret store** (k8s Secret, Fly secrets, AWS Secrets Manager). Not from `.env` files in containers. `.env` is a dev-only convenience.
- **Fail fast on missing config.** Binaries should `os.Exit(1)` at startup with a clear `missing env var: FOO` message if a required env var isn't set. Don't pretend it's optional and fail mysteriously later.
- **Sensible defaults for optional config.** Timeouts, intervals, pool sizes — env var overrides a hardcoded sane default. `envOr("GOTENBERG_URL", "http://gotenberg:3000")` is the pattern.

## Maintainability

- **Small files, focused packages.** If an `internal/<pkg>/<file>.go` grows past ~300 lines, consider splitting. Each file should be readable in one scroll.
- **Package boundaries reflect domains, not tech layers.** We have `internal/gmail`, `internal/drive`, `internal/pdf`, `internal/store`, `internal/queue`, `internal/oauth`. We do NOT have `internal/models`, `internal/services`, `internal/utils`.
- **Cross-package state crosses boundaries through typed APIs, not global vars.** No init-time mutations of package-level state. Construction happens in `main.go` / `cmd/*`; internals take dependencies via `Config` structs.
- **Hand-written SQL in `internal/store`.** No ORM, no reflection magic. See the existing `EnqueueJob` / `GetJob` pattern.

## Library choices (the hand-rolled-on-purpose project)

The point of this project is to demonstrate knowing the internals. Don't pull in libraries that hide them.

- **Don't use:** Asynq, Machinery, River, go-workers, gocraft/work, or any other queue library. The whole point is writing the queue ourselves on raw Redis.
- **Don't use:** Gin, Echo, Fiber, or any heavyweight web framework. `net/http` is enough; add `chi` if routing gets messy.
- **Don't use:** GORM, ent, sqlboiler, or any ORM. Hand-write SQL with `pgx`. Migrations via `golang-migrate`.
- **Don't use:** OpenTelemetry, Datadog SDK, or any heavy observability stack in v1. `log/slog` with JSON handler. Prometheus client only for the queue-depth metric the HPA needs.
- **Use:** `pgx/v5`, `go-redis/v9`, `chi` (only if needed), `slog`, `golang-migrate`, `testcontainers-go`. That's it for the core.

## Layout

Standard Go layout, no surprises.

```
cmd/
  api/         # HTTP API + dashboard
  scheduler/   # Gmail History poller
  worker/      # Single binary, --stage flag selects fetch/render/upload
  sweeper/     # Heartbeat sweeper
internal/
  queue/       # Redis queue ops (LPUSH/BLPOP/sweep)
  store/       # Postgres access (pipeline_jobs, history, oauth, processed_emails)
  pipeline/    # Job model + stage logic
  gmail/       # Gmail client + History sync
  drive/       # Drive client + folder cache + uploader
  pdf/         # Gotenberg client
  oauth/       # Token storage + AES-GCM
  dashboard/   # SSE feed + demo triggers
  testutil/    # testcontainers helpers (one place; not duplicated per package)
deploy/k8s/    # k3s manifests
.github/workflows/
docs/superpowers/
```

Worker is one binary with a `--stage` flag because the three stages share heartbeat + claim + retry plumbing. Same image, three deployments.

## Output / chat / commits

Global rules cover most of this. Project additions:

- When reporting test results, paste the actual output — don't paraphrase. "All tests pass" is not evidence; `ok internal/queue 0.412s` is.
- Don't claim k3s deploys work without `kubectl get pods` output showing it.
- **Commit subjects use Conventional Commits** with optional component scope: `<type>(<scope>): <subject>`. Examples: `feat(api): add SSE stream endpoint`, `fix(queue): handle BLPOP timeout race`, `chore(k8s): bump postgres image to 16.3`. Bare `chore:` / `docs:` / `test:` (no scope) are also fine. Allowed types: `feat`, `fix`, `chore`, `docs`, `test`, `refactor`, `perf`, `build`, `ci`, `style`, `revert`. Scopes are project components: `api`, `queue`, `worker`, `sweeper`, `scheduler`, `gmail`, `drive`, `k8s`, `oauth`, `dashboard`. Subject in imperative mood, under 72 chars, no trailing period.

## What I do NOT want, ever

- Tests written after the code "just to have coverage."
- "Helpful" wrappers around `pgx` or `go-redis` that re-invent half the library.
- Generated boilerplate (sqlc, protobuf, etc.) without explicit ask.
- AI prose in PR descriptions or commit bodies.
- Emoji status lines, "✓ complete!" output, summaries dressed up with tables when prose would do.
- **Real secrets in `.env.example`** or anywhere in the repo. Placeholders only.
- **Hardcoded URLs or credentials** anywhere. Even for "just testing" — tests set env vars if they need them.
- **Global singletons** instead of Config-based construction. Every unit takes its dependencies as a parameter.
