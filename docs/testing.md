# Test strategy and verification scope

The test suite prioritizes the example workflow and its transactional failure
boundaries. Domain tests exercise matching, email extraction and valid/invalid
state transitions. HTTP tests exercise authentication and malformed requests.
Integration tests use real PostgreSQL and the real persistent simulator; no
in-memory repository replaces the database.

Run from the repository root:

```sh
docker compose up -d db
export TEST_DATABASE_URL='postgres://blotato:local-postgres-only@localhost:55432/blotato?sslmode=disable'
go test ./...
go test -race ./...
go vet ./...
```

Use the matching database password if the Compose default was changed. The
integration database role must be able to create schemas. Each test creates a
random schema, applies real migrations, then drops only its own schema. Tests do
not truncate the shared public schema. Without `TEST_DATABASE_URL`, PostgreSQL
tests explicitly skip; a passing unit-only run does not establish persistence
or concurrency correctness. CI configures PostgreSQL and sets this variable.

| Behavior | Coverage |
| --- | --- |
| Complete workflow | HTTP create/get automation, comment, ordered reply and prompt, invalid email, valid email, final link, completed state, replay |
| Deduplication | Repeated event ID, same logical object under a new event ID, changed payload conflicts, no extra logical actions |
| Correlation | Wrong user/account/tenant, one active conversation, oldest matching automation, suppressed triggers stay suppressed on replay |
| Ordering | Email during initial delivery is captured, final waits for prompt acknowledgment; pre-trigger DM remains unmatched |
| Immutable configuration | Disabling affects future runs; an existing snapshot finishes |
| Concurrency | 16 simultaneous claims, triggers and emails; locked row skipping; two worker loops; prompt acknowledgment racing a valid DM |
| Failures | Temporary failure, ambiguous timeout after success, and permanent rejection on all three action kinds; email-request failures after early email capture; final delivery remains pending until acknowledgment; bounded attempts and crash on the final attempt |
| Recovery | Abandoned committed lease before send and after successful send, new connection pool/repository/worker, stale acknowledgment and failure fencing |
| Atomicity | Inbox failure rolls back execution/outbox/receipt; next-action failure rolls back acknowledgment while remote effect remains |
| Expiry | Late email cannot revive a timed-out session; sweeper releases the active conversation |
| Database invariants | Migration replay/down/up, tenant foreign keys, active conversation uniqueness, unique action kind and required email for completion |
| Privacy | Execution inspection omits the captured email; inbound text is not stored as an event body |

The recovery tests simulate a crash by leaving a committed claim unacknowledged,
expiring its lease in SQL, and constructing a fresh pool, repository and worker.
They exercise the same durable recovery path without waiting for wall-clock
lease expiry. They do not kill an operating-system process or interrupt a
PostgreSQL server mid-commit. The simulator's independent transaction reproduces
the critical external-success/local-acknowledgment boundary, but cannot prove
idempotency of a real Instagram adapter. No claim is made about real network
delivery, provider API quotas, load capacity, PostgreSQL failover, or production
webhook signature verification.

## Recorded verification

The following results were recorded on 2026-10-26 with Go 1.26.10 and
PostgreSQL 17. They describe that verification run.

| Check | Result |
| --- | --- |
| Formatting and `go vet ./...` | Passed |
| `go test -count=1 ./...` with `TEST_DATABASE_URL` | All packages passed, including integration and migration tests |
| `go test -race -count=1 ./...` with `TEST_DATABASE_URL` | All packages passed; no reported races |
| `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` | No vulnerabilities found after dependency/toolchain updates |
| `docker compose config --quiet` and container build/startup | Passed; application and database became healthy; application ran as non-root |
| `python3 scripts/demo.py` | Ordered reply/prompt/link, invalid email waiting, completion, duplicate replay, exactly three simulated effects |
| `python3 scripts/demo.py --restart --failure after_success` | Application restart while waiting, persisted-state recovery, ambiguous-delivery retry, exactly three simulated effects |
| Fresh Compose project and database volume | Migration, account bootstrap, and restart/failure scenario passed |

To reproduce the fresh-volume scenario with separate ports and storage:

```sh
COMPOSE_PROJECT_NAME=blotato-clean HTTP_PORT=18080 PG_PORT=55433 \
  docker compose up --build -d --wait
COMPOSE_PROJECT_NAME=blotato-clean HTTP_PORT=18080 PG_PORT=55433 \
  python3 scripts/demo.py --base-url http://localhost:18080 --restart --failure after_success
```

The hosted GitHub Actions run was not verified. Its test/build/demo commands
were exercised. PostgreSQL power-loss/failover, real Instagram delivery,
production webhook verification, and load capacity were not tested.
