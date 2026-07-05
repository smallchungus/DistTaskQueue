# Self-hosting

Run the full Gmail → PDF → Drive pipeline on your own machine with Docker
Compose. About 10 minutes, most of it clicking through Google Cloud Console.

Prereqs: Docker, Docker Compose v2.24+ (required for optional `env_file` support).

## 1. Google Cloud setup

The pipeline reads Gmail via the Gmail API and writes PDFs to Drive via the
Drive API, both under an OAuth 2.0 client you own.

1. **Create or reuse a project** at [console.cloud.google.com](https://console.cloud.google.com).
2. **Enable APIs:** in "APIs & Services" → "Library", enable the **Gmail API**
   and the **Drive API**.
3. **OAuth consent screen:** set scopes to `gmail.readonly` and `drive.file`.
   Add your own Google account as a test user.

   **Publish to production before you rely on this long-term.** In Testing
   mode, Google expires refresh tokens after 7 days and the pipeline goes
   dark with no visible error — the scheduler logs a warning per poll and
   nothing else notices. This bit us in production once; see
   [WAR-STORIES.md #1](./WAR-STORIES.md). Publishing removes the 7-day
   expiry. A personal single-user OAuth app does not need Google's
   verification review to publish.
4. **Create an OAuth 2.0 Client ID**, type **Desktop app**. Add
   `http://localhost:8888/callback` as an authorized redirect URI. Note the
   client ID and secret — you'll need both in the next step.

## 2. Configure

```bash
cp .env.example .env
```

Edit `.env`:

- `GOOGLE_OAUTH_CLIENT_ID` / `GOOGLE_OAUTH_CLIENT_SECRET` — from step 1.
- `TOKEN_ENCRYPTION_KEY` — generate with `openssl rand -base64 32`.
- `DRIVE_ROOT_PATH` (optional) — slash-delimited folder path under which
  dated backup folders are created, e.g. `02_GmailBackup/Gmail Backup`.
  Leave empty and PDFs land at the Drive root.

Leave `DATABASE_URL` and `REDIS_URL` as-is — Compose points every service at
the `postgres` and `redis` containers directly; those two entries in `.env`
only matter if you run a binary outside Compose.

## 3. Pinning a version (optional)

By default, Compose pulls `disttaskqueue-api:latest`. For production, pin to a
released version from the [releases page](https://github.com/smallchungus/DistTaskQueue/releases).
Edit `docker-compose.yaml` and replace all occurrences of `:latest` with the
version tag, e.g., `:v0.1.0`.

## 4. Bring up the stack

```bash
docker compose up -d
```

This starts Postgres, Redis, Gotenberg, the API, all three worker stages
(fetch/render/upload), the scheduler, and the sweeper. Check everything is
up:

```bash
docker compose ps
curl localhost:8080/healthz
```

## 5. Authorize your Google account

Run the one-off OAuth bootstrap. It opens a local callback server on
`:8888` and prints a URL to open in your browser:

```bash
docker compose run --service-ports oauth-setup --email=you@example.com
```

Open the printed URL, sign in, authorize. The terminal prints "Token saved"
once the callback completes. The scheduler starts syncing that account on
its next poll (every 60 s).

## 6. Verify it works

Send yourself an email. Within about 2 minutes it should show up as a PDF
in your Drive, under a dated folder tree:
`<DRIVE_ROOT_PATH>/YYYY/Month YYYY/DD Month YYYY (Weekday)/<your email>/`.

If it doesn't, `docker compose logs -f scheduler worker-fetch worker-render
worker-upload` is the first place to look.

## 7. Back up your existing inbox

The scheduler only forward-syncs new mail — it never backfills on its own,
so day one only picks up whatever arrives after you connect the account.
To pull in your existing personal mail, including anything archived out of
the inbox, run the one-shot backfill command:

```bash
docker compose run backfill --email=you@example.com --since=2020-01-01
```

`--since` and `--before` take `YYYY-MM-DD` dates and are both optional;
omit `--since` to go back to the start of the mailbox. Gmail's search
matches whole days in the account's local timezone, not exact timestamps,
so messages right at a day boundary may land in either run. It pages through
`messages.list`, enqueues a `fetch` job per message with the same
idempotency check the scheduler uses, and pauses whenever `queue:fetch`
gets more than 500 jobs deep (`--max-queue` to change the cap) so it can't
outrun the workers.

Expect roughly 1-2 emails/second per render worker — Gotenberg's Chromium
render is the bottleneck here, not the Gmail API. To go faster, scale the
render worker: `docker compose up -d --scale worker-render=3` (on k3s, use
the KEDA HPA described in OPERATIONS.md).

Safe to interrupt with Ctrl+C and re-run — already-enqueued messages are
skipped on the next pass.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Scheduler logs `invalid_grant` repeatedly | OAuth consent screen still in Testing mode — refresh tokens expire after 7 days | Publish the app to production (step 1.3), then re-run `oauth-setup` |
| Emails sync but the Drive folder is empty or in the wrong place | `DRIVE_ROOT_PATH` / `DRIVE_ROOT_FOLDER_ID` unset or pointing elsewhere | Set `DRIVE_ROOT_PATH` in `.env`, restart the upload worker: `docker compose restart worker-upload` |
| `docker compose up` fails with "port is already allocated" | Something else on your machine already uses 5432, 6379, 3000, 8080, or 8888 | Stop the conflicting process, or remap the host side of the port in `docker-compose.yaml`, e.g. `"15432:5432"` |
| A binary exits immediately with `missing env var: FOO` | `.env` is missing a required value | Fill in the var it names — this is fail-fast by design, not a bug |

## Production-grade option

Docker Compose is the fast path for a single machine. For autoscaling,
TLS, backups, and metrics, see the Kubernetes deployment under
`deploy/k8s/` and the runbooks in [OPERATIONS.md](./OPERATIONS.md).
