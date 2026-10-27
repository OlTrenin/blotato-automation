# Security review and deployment boundary

This service runs locally with a working simulated platform. The
operator API and event endpoints require a tenant-scoped bearer credential.
They are intended for an already authenticated platform ingress adapter, not for
raw public Instagram webhooks. Possession of a tenant credential authorizes
submitting that tenant's events and changing its automation configuration.

## Implemented controls

- `/v1/` routes authenticate before dispatch. The server derives tenant identity
  from its credential mapping; request JSON cannot choose a tenant. The example
  binary configures one tenant credential, while the HTTP handler supports
  multiple credentials for isolation tests. Configured credentials are hashed
  in the handler lookup table and never logged.
- Ingestion requires an existing connected account for that tenant/platform.
  Tenant-facing repository queries are scoped. Composite foreign keys prevent
  cross-tenant account, automation, and execution references. Worker access is
  internal and intentionally spans tenants.
- SQL values use parameters. JSON rejects unknown fields and trailing bodies,
  has a 64 KiB request limit, validates the original UTF-8 and escaped Unicode,
  rejects duplicate names/case aliases, and applies smaller text/identifier limits.
  Identifiers exclude control characters. Network and request timeouts bound
  individual requests.
- The email parser accepts one distinct syntactically valid unquoted ASCII
  mailbox with a DNS-style domain. Multiple different mailboxes are ambiguous
  and do not advance the workflow. Validation does not establish mailbox
  ownership, deliverability, consent, or a verified customer identity.
- Collected email is not included in inspection JSON or outbound messages.
  Inspection exposes the historical `email_received` boolean. Raw inbound text
  is discarded after transactional processing. Logs use event/execution/action
  identifiers, outcomes, states, and safe error classes; provider/database
  response text and bearer credentials are omitted.
- Configured links require HTTPS without embedded credentials. The engine
  sends the link as message text; it never fetches that URL, so this path does
  not perform a server-side request to a customer-selected destination.
- Provider event IDs and logical comment/message IDs are deduplicated within
  tenant/account/event kind. Changed content under a reused identity is rejected.
  This protects against repeated deliveries of accepted events; it does not
  authenticate the origin of a newly invented event.
- Failure-injection and simulator-inspection endpoints are registered only
  when `DEMO_MODE=true`, and still require authentication. Compose publishes
  application and database ports on loopback. `.env` is excluded from Git and
  the image build context. The application container runs as a non-root user.

## Review findings and accepted local limitations

No critical or high-severity security defect was identified within the stated
local-demo trust boundary. This was a code review; it is not a penetration
test or certification.

| Severity | Finding | Consequence / required production work |
| --- | --- | --- |
| Medium | `executions.email` is plaintext and has no automatic retention job. | Protect database storage/backups, implement erasure and a documented retention policy, and consider envelope encryption with managed keys. The sample contains no real customer data. |
| Mitigated | Unbounded lists and tenant growth. | Lists are capped at 100 with keyset pagination; a tenant has admission caps of 1,000 automations, 100,000 events and 200,000 receipts, atomically persisted in PostgreSQL. Per-process authenticated request limiting is 100/s with burst 200. Distributed ingress throttling and capacity monitoring remain deployment concerns. |
| Medium | Development bearer credentials, HTTP, disabled database TLS, and the Compose database owner role are local conveniences. | Use TLS, managed/rotatable secrets, independently verified provider ingress, separate least-privilege runtime/migration database roles, and restricted network access. Do not deploy the Compose defaults as production credentials. |
| Low | Content hashes and social identifiers are retained indefinitely for replay correctness. | SHA-256 is deduplication evidence, not anonymization: guessable message content can be tested against a hash. Protect these records as personal data and establish a retention/replay policy. |

`ListAutomations`, `ListExecutions`, and `ListOutbound` now use bounded cursor
pagination. Automation matching scans pages of 100 and stops at the oldest match;
existing tenants above the new definition cap keep working after upgrade.
Queue/table-growth monitoring remains an operational requirement.

The ingress adapter is responsible for validating provider signatures,
timestamps/replay windows where available, the connected account, post
ownership/eligibility, and genuine sender identity before forwarding an event.
The service assumes those checks have already occurred. A valid local bearer
credential can submit invented events for its own tenant. Separate operator and
ingress permissions are a production follow-up.

The simulator remains the binary's only outbound adapter even when its HTTP
inspection/control routes are disabled. Turning off `DEMO_MODE` does not create
a production Instagram integration.

## Retention without silently reopening historical triggers

There is no automatic purge. Terminal email values can be redacted without
deleting replay evidence. After taking a backup according to the applicable
data policy, an operator can run this deliberately scoped example:

```sql
BEGIN;
UPDATE executions
SET email = ''
WHERE state IN ('COMPLETED', 'FAILED', 'EXPIRED')
  AND updated_at < now() - interval '30 days'
  AND email IS NOT NULL
  AND email <> '';
COMMIT;
```

The empty string is a redaction sentinel for terminal rows. It preserves the
current non-null completion invariant and the historical `email_received`
boolean; terminal executions do not need the mailbox to deliver another action.
Do not redact active rows. This query is an operational example, not an
implemented scheduler, and backup copies require their own retention policy.

Keep `event_receipts` and canonical `inbound_events` while promising the current
indefinite duplicate suppression. Deleting their keys can allow an old comment
to start again. A bounded production retention window needs an explicit replay
contract, trusted provider event times or another freshness boundary, and
coordinated erasure of related records. An unkeyed hash alone does not solve
privacy retention.

## Verification scope

The HTTP security tests cover missing/wrong credentials, credential-derived
tenant scope, copied authentication configuration, invalid/oversized JSON,
disabled development routes, and sensitive database error suppression. The
PostgreSQL integration suite exercises tenant/account/user separation and
database foreign-key enforcement. Refer to [testing.md](testing.md) for the
executed suite and its limits; no external-provider authenticity or production
network security testing is claimed.

## Audit remediation, 27 October 2026

The email extractor keeps quoted constructs intact and never accepts an address
fragment from a quoted local part. Link validation checks DNS labels, IP literals
and TCP port bounds without making network requests. Content-Type parsing follows
media type case rules. See the [remediation evidence](testing.md#audit-remediation-verification--2026-10-27).

Tenant buckets are allocated only for configured identities, avoiding unbounded
IP/credential-keyed limiter maps. Limits apply before obtaining DB connections;
they are per process. Storage admission counters and associated changes commit
in the same transaction across replicas. Counters are initialized from existing
rows by migration 002; failed admissions and exact replays do not inflate them.
These controls bound growth, not provider throughput or historical-data retention.
