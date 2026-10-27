# Blotato comment automation

A runnable Go service for one complete automation: a matching Instagram comment
receives a reply and email-request DM; a valid DM response triggers a configured
link. PostgreSQL persists definitions, inbound identities, workflow state, and
pending actions. No real Instagram credentials are required.

The design is a modular monolith with a fixed state machine and a PostgreSQL
transactional outbox. This keeps workflow changes and action scheduling atomic,
and supports multiple workers without introducing a broker or workflow platform.
The simulated social adapter records deliveries and supports controlled failures.

## Start and demonstrate

Prerequisites: Docker with Compose v2. For the demo script, Python 3; for host
development and tests, Go 1.26.10 (the declared toolchain). Ports 8080 and 55432
must be available. Go can download that toolchain when automatic toolchain
selection is enabled.

```sh
cp .env.example .env
docker compose up --build -d --wait
python3 scripts/demo.py
python3 scripts/demo.py --restart --failure after_success
```

The second demo restarts the actual application container while waiting for an
email and retries an email-request call whose simulated remote effect succeeded
before its response failed. Both runs inspect the real HTTP API and durable
state, assert ordered delivery and completion, and replay duplicate events.
Each run uses unique object IDs and a unique `pricing…` keyword so earlier demo
definitions cannot intercept it. Run failure demos sequentially: failure controls
apply to the tenant and action kind. The created automation is disabled afterward;
its definition, execution, and three outbound records remain inspectable.

Compose provisions `tenant-demo` and its connected Instagram account
`account-123`, applies the embedded migration, and starts two workers. Data lives
in the `postgres-data` volume. `docker compose down` preserves it; the explicit
destructive reset is `docker compose down -v`.

Defaults in `.env.example` are local credentials. Compose binds both published
ports to loopback. If you change `API_TOKEN`, export the same value when running
the Python script. `.env` is ignored by Git; Compose reads it, while Python and Go
read their process environment.

## Complete curl walkthrough

Use a fresh database for these fixed IDs, or change the IDs for another run.
The first enabled automation matching `pricing` wins; disable an older one if
you previously created a different `pricing` automation.

```sh
export API_TOKEN=local-demo-token-change-me
export BASE_URL=http://localhost:8080

# 1. Create the persisted automation. Save its returned id for later inspection.
curl -fsS "$BASE_URL/v1/automations" \
  -H "Authorization: Bearer $API_TOKEN" -H 'Content-Type: application/json' \
  -d '{"platform":"instagram","account_id":"account-123","keyword":"pricing","comment_reply":"Sent you a DM!","email_prompt":"What is your email address?","link":"https://example.com/pricing"}'

# 2. Send a matching comment. Save the returned execution_id.
curl -fsS "$BASE_URL/v1/events/comments" \
  -H "Authorization: Bearer $API_TOKEN" -H 'Content-Type: application/json' \
  -d '{"event_id":"comment-event-001","platform":"instagram","account_id":"account-123","post_id":"post-456","comment_id":"comment-789","user_id":"user-001","text":"pricing"}'

export EXECUTION_ID=REPLACE_WITH_RETURNED_EXECUTION_ID

# 3. Poll these until state is WAITING_FOR_EMAIL and two effects are visible.
curl -fsS "$BASE_URL/v1/executions/$EXECUTION_ID" -H "Authorization: Bearer $API_TOKEN"
curl -fsS "$BASE_URL/v1/simulator/outbound" -H "Authorization: Bearer $API_TOKEN"

# 4. Invalid email: outcome invalid_email, no additional message.
curl -fsS "$BASE_URL/v1/events/messages" \
  -H "Authorization: Bearer $API_TOKEN" -H 'Content-Type: application/json' \
  -d '{"event_id":"invalid-event-001","platform":"instagram","account_id":"account-123","user_id":"user-001","message_id":"invalid-message-001","text":"Not yet"}'
curl -fsS "$BASE_URL/v1/executions/$EXECUTION_ID" -H "Authorization: Bearer $API_TOKEN"

# 5. Valid email: final DM contains the configured link.
curl -fsS "$BASE_URL/v1/events/messages" \
  -H "Authorization: Bearer $API_TOKEN" -H 'Content-Type: application/json' \
  -d '{"event_id":"message-event-001","platform":"instagram","account_id":"account-123","user_id":"user-001","message_id":"message-789","text":"My email is hello@example.com"}'

# 6. Poll until COMPLETED; outbound now contains three effects for this execution.
curl -fsS "$BASE_URL/v1/executions/$EXECUTION_ID" -H "Authorization: Bearer $API_TOKEN"
curl -fsS "$BASE_URL/v1/simulator/outbound" -H "Authorization: Bearer $API_TOKEN"

# 7. Replay the same comment and DM curl requests above.
# Each response has duplicate:true and the original execution_id; effects stay at 3.
```

Waiting does not hold a worker or connection. To inspect persisted state directly:

```sh
docker compose exec db psql -U blotato -d blotato -c \
  'SELECT id,state,email IS NOT NULL AS email_received,expires_at FROM executions;'
docker compose exec db psql -U blotato -d blotato -c \
  'SELECT execution_id,kind,status,attempts,last_error FROM actions ORDER BY created_at;'
docker compose logs app
```

## API

All `/v1/` routes require `Authorization: Bearer <API_TOKEN>`. The credential
determines the tenant; request bodies cannot select another tenant.
These event routes model an already verified upstream platform receiver, not
unsigned public Instagram webhooks. Account ownership and eligible-post
verification belong to that existing receiver.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/healthz` | Process liveness, public |
| GET | `/readyz` | Database readiness, public |
| POST / GET | `/v1/automations` | Create / list definitions |
| GET | `/v1/automations/{id}` | Inspect definition |
| PATCH | `/v1/automations/{id}` | Set `{"enabled":false}` or `true` |
| POST | `/v1/events/comments` | Receive the comment contract above |
| POST | `/v1/events/messages` | Receive the DM contract above |
| GET | `/v1/executions` | List workflow state |
| GET | `/v1/executions/{id}` | Inspect state and ordered action records |
| GET | `/v1/simulator/outbound` | Inspect simulated effects; demo only |
| POST | `/v1/simulator/failures` | Control simulated failures; demo only |

JSON bodies require `Content-Type: application/json` (media type comparison is case-insensitive).
Duplicate fields, case aliases, malformed UTF-8 and unpaired Unicode surrogates are rejected.
Field names use the documented lowercase spelling. Validation rejects unknown
fields, multiple JSON values, invalid IDs, and oversized input. Creation returns
201; committed event ingestion returns 200. Errors use 400 for invalid input,
401 for missing credentials, 404 for an unknown or inaccessible resource, 409 for
reusing an event/object identity with changed content, 413 for an oversized body,
415 for unsupported content type, 429 for request-rate or tenant admission limits, and 500 for storage failures.

Example failure control, consumed by the next matching adapter attempt:

```sh
curl -fsS "$BASE_URL/v1/simulator/failures" \
  -H "Authorization: Bearer $API_TOKEN" -H 'Content-Type: application/json' \
  -d '{"kind":"request_email","mode":"transient","count":1}'
```

Kinds are `reply_comment`, `request_email`, and `send_link`; modes are `transient`,
`permanent`, and `after_success`. Count is 0–20. `after_success` records the remote
effect and then returns an error; its retry uses the same idempotency key.

## Tests and host development

```sh
docker compose up -d --wait db
export TEST_DATABASE_URL='postgres://blotato:local-postgres-only@localhost:55432/blotato?sslmode=disable'
go test -count=1 ./...
go vet ./...
go test -race -count=1 ./...
test -z "$(gofmt -l cmd internal integration)"
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

Database-backed tests create and drop their own schemas. Use a disposable test
database and a role permitted to create schemas. Without `TEST_DATABASE_URL`,
those tests explicitly skip; a unit-only pass does not establish concurrency or
durability. CI supplies real PostgreSQL and also runs the containerized demo.
See [testing.md](docs/testing.md) for behavioral coverage and executed evidence.
The full suite, full race-detector scope, vulnerability scan, container startup,
and fresh-volume restart demo passed locally; recorded results and verification
limits are in [testing.md](docs/testing.md).

To run the application on the host instead of in its container:

```sh
docker compose stop app
docker compose up -d --wait db
set -a
. ./.env
set +a
go run ./cmd/server
```

The local `.env` uses the published database port 55432. Migrations are embedded
in the binary and run transactionally at startup under a database advisory lock.
The down migration is included for test/disposable environments; application
startup only migrates forward.

| Configuration | Default / requirement |
| --- | --- |
| `DATABASE_URL` | Required; host example in `.env.example` |
| `API_TOKEN` | Required, at least 16 bytes, no whitespace |
| `DEMO_MODE` | `false`; Compose enables demo bootstrap and simulator endpoints |
| `TENANT_ID`, `ACCOUNT_ID` | `tenant-demo`, `account-123` |
| `ADDR` | `127.0.0.1:8080` on the host; Compose uses `:8080` internally |
| `WORKERS` | `2`, allowed 1–32 |
| `WAIT_TTL` | `24h`, allowed 1s–720h; begins at prompt acknowledgment |

The supplied binary always uses the simulated adapter. `DEMO_MODE=false` removes
its inspection/failure routes and automatic account provisioning; it does not
install a real Instagram connector.

## Semantics and limits

- Matching uses a case-insensitive whole Unicode letter/number token.
  `PRICING!` matches `pricing`; `repricing` and `pricing2` do not.
- One active execution exists per tenant/platform/account/user across all
  automations. Oldest matching enabled definition wins, then ID. Further comments
  during that session are consumed and suppressed, never queued for later.
- Definitions are immutable except for enabled; executions retain a snapshot.
  Disabling affects new matches. Replaying an old comment cannot restart it;
  a genuinely new comment after completion can start another execution.
- Event and logical object identities are durable. Changed content under a known
  identity is a conflict. Invalid DMs keep waiting without a clarification. Email
  syntax checking accepts one distinct unquoted ASCII mailbox with a DNS-style
  domain; it does not verify ownership or deliverability.
- A valid email received after execution creation is captured even before prompt
  acknowledgment; the final link remains ordered after the initial actions.
  A DM committed before any session stays unmatched. There is no unsafe replay
  of historical email into a future workflow.
  A previously unseen delayed reply received during a later active session is
  treated as a reply to that session; distinguishing its original conversation
  requires provider thread/reply identifiers absent from this event contract.
- State changes and action creation commit atomically. Workers use `SKIP LOCKED`,
  expiring leases, and ownership tokens. Default delivery budget is five attempts,
  5s call timeout, 15s lease, and exponential retries with jitter. A crash on the
  final attempt is eventually terminalized. Waiting sessions expire durably.
- Completion means the final adapter call was acknowledged. External delivery
  is at least once; real-provider timeouts can produce duplicates or a visible
  effect on an execution later marked failed. The simulator deduplicates stable
  action IDs; that capability is not a universal Instagram guarantee.
- Structured logs carry event/execution/action IDs and safe error classes. Raw
  DM bodies and email addresses are absent from logs and inspection responses.
  The collected email remains private plaintext in PostgreSQL. Retention, erasure,
  production identity management, TLS, verified webhooks, provider quotas, and
  monitored alerting require production integration work.

The service supports a fixed comment-to-email workflow. All list endpoints return
at most 100 records, ordered by creation time and ID. Use `?limit=1..100` and pass
the returned `X-Next-Cursor` header as the next request's `after` parameter.
Responses remain JSON arrays. A full last page may point to an empty next page.
The demo script follows all pages when inspecting effects.

Each process admits 100 authenticated requests/second per tenant with a burst of
200, shared across that tenant's credentials; excess requests return 429 with
`Retry-After: 1`. PostgreSQL admission quotas are shared across all processes:
1,000 automation definitions, 100,000 canonical inbound events and 200,000 event
receipts per tenant. Exact replays require no new quota; a new event-ID alias
consumes one receipt. Quota failure rolls back the whole workflow transaction.
Admission counters persist across restarts and do not reset automatically.
Disable/re-enable does not free a definition slot. Changing these caps or pruning
replay history requires a deliberate administrative data policy.

There is no public registration, OAuth, arbitrary workflow graph,
broker, or Kubernetes dependency. [Security](docs/security.md) and
[reliability](docs/reliability.md) document the remaining operating concerns.

## Review map

- [Architecture and sequence diagrams](docs/architecture.md)
- [Data model and database invariants](docs/data-model.md)
- [Architectural decisions and alternatives](docs/decisions.md)
- [Reliability and crash boundaries](docs/reliability.md)
- [Security and deployment requirements](docs/security.md)
- [Tests and recorded verification](docs/testing.md)
- [AI usage](docs/ai-usage.md)
- `internal/domain`: matching, validation, explicit state transitions
- `internal/postgres`: atomic workflow use cases, migrations, simulator persistence
- `internal/app`: delivery ports and recoverable workers
- `internal/httpapi`, `cmd/server`: transport, configuration, process lifecycle
- `integration`: HTTP and real-PostgreSQL behavioral tests
