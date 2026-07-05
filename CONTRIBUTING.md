# Contributing

Thanks for looking. This is a real project people self-host, so the bar is production code, not demo code.

## Setup

```bash
git clone https://github.com/smallchungus/DistTaskQueue
cd DistTaskQueue
make test-unit          # fast, no Docker
make test-integration   # real Postgres + Redis via testcontainers-go (needs Docker)
make lint               # golangci-lint
```

To run the whole stack locally, see [docs/SELF-HOSTING.md](docs/SELF-HOSTING.md).

## Ground rules

- **Tests before code.** Integration tests use real Postgres and Redis via testcontainers — no mocking the queue or the database. Outbound HTTP (Gmail, Drive, Gotenberg) is faked with `httptest`.
- **Conventional Commits** with an optional component scope: `feat(queue): ...`, `fix(drive): ...`, `docs: ...`. Subject in imperative mood, under 72 characters.
- **One concern per pull request.** Two unrelated changes are two PRs.
- **No new queue/web/ORM dependencies.** The point of the project is owning the internals — raw `go-redis`, hand-written SQL over `pgx`, `net/http`. See [CLAUDE.md](CLAUDE.md) for the full house rules.

## Reporting bugs and security issues

Regular bugs: open an issue with the smallest reproduction you can manage. Security issues: see [SECURITY.md](SECURITY.md) — report privately, not in a public issue.
