# Adoption Polish Implementation Plan

**Goal:** Close the last self-hosting friction and make the repo star-worthy at first glance.

## Task A — idempotent uploads (replaces the processed_emails promise)

Files: internal/drive/client.go (Upload), internal/drive/client_integration_test.go, docs/ARCHITECTURE.md + spec (idempotency paragraphs), SECURITY.md if it mentions duplicates.

1. TDD: failing test against the existing Drive httptest fake — uploading the same name into the same parent twice yields ONE file (second call returns the existing id, no second create).
2. Upload becomes find-before-create, same query shape as EnsureFolder (name + parent + trashed=false, non-folder mime not required — name+parent suffices for our layout since each email owns its folder).
3. Docs: ARCHITECTURE §6.1 and the spec describe idempotency as (a) enqueue-time unique index, (b) find-before-create uploads; processed_emails noted as present-but-unused in the schema. Portfolio page claim already matches "idempotent side effects" generically — no change there.
4. Commit: `fix(drive): make uploads idempotent so retries never duplicate files`

## Task B — no-clone quickstart

Files: docker-compose.yaml (only if image refs need it — expect none), docs/SELF-HOSTING.md (top section), README.md (quickstart pointer).

1. SELF-HOSTING opens with the no-git path: curl the raw docker-compose.yaml and .env.example from GitHub main, fill .env, `docker compose up -d`. Clone path demoted to "developing/building from source."
2. Verify the curl path actually works into a scratch dir: compose config parses standalone (no repo files assumed — check for any build-context/relative-path assumptions that break outside the repo).
3. README quickstart shows the same two curls.
4. Commit: `docs: no-clone quickstart via raw compose download`

## Task C — star-worthiness (coordinator-executed)

- gh: repo description + topics (gmail-backup, self-hosted, golang, task-queue, kubernetes, drive, docker).
- Dashboard screenshot (live prod via playwright) committed as docs/dashboard.png, embedded at README top with the one-line pitch.
- Badges: license + latest release + Go Report Card.
- CONTRIBUTING.md: thin — dev setup, test commands, conventional commits, one-concern PRs.
- Launch drafts (chat only): r/selfhosted + Show HN.

User-gated: GHCR package public, v0.1.0 tag (after A+B merge), Security Advisories, consent-screen publish, social-preview upload.
