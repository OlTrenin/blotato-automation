# Architectural decisions

These decisions target the complete email-collection workflow.
They favor durable correctness and a small reviewable system over a general
automation platform.

## 1. A fixed workflow in a modular monolith

**Chosen:** Go HTTP handlers, workflow application services, a persistence
adapter, and background workers in one deployable service. Automations configure
one explicit workflow rather than arbitrary steps.

**Why:** The workflow has one concrete branching condition: whether a DM
contains an acceptable email. An explicit state machine makes valid transitions,
failure recovery, and tests easy to inspect. Keeping transport, workflow rules,
persistence, and platform delivery separate preserves useful boundaries without
requiring distributed services.

**Rejected:** A generic graph interpreter, user-supplied code, microservices, and
Temporal. They add execution semantics, deployment dependencies, and security
surface without improving this use case. A general engine becomes reasonable
when multiple real workflows need different control flow.

**Consequence:** Adding a different kind of automation requires code and a
schema/API decision. This is an intentional scope boundary.

## 2. PostgreSQL as the durable coordinator

**Chosen:** PostgreSQL stores definitions, inbound events, executions, pending
actions, and delivery attempt metadata. Short transactions, uniqueness
constraints, row locks, and leases coordinate workers.

**Why:** Workflow state and logical side effects can commit atomically. There is
no database-to-broker dual write, no broker deployment, and no ephemeral session
state to recover after restart. `SKIP LOCKED` allows independent workers to make
progress without waiting on already claimed work.

**Rejected:** In-memory queues and sleeping goroutines cannot survive restarts.
Redis or a message broker would introduce another failure boundary and still
need an outbox to coordinate database changes. Their throughput and fan-out
benefits have no demonstrated need here.

**Consequence:** Queue polling and durable history consume database capacity.
Operational growth will require measured tuning and retention, potentially an
outbox publisher to a broker later.

## 3. Transactional, synchronous inbound processing

**Chosen:** Persist and process each supported inbound event in one short
transaction, then return its durable outcome.

**Why:** Matching and parsing require local computation and database reads with
bounded event bodies. A separate inbox worker would add event lease recovery
and ordering problems without removing meaningful work from the request. The
persisted inbox remains useful for deduplication and explaining ignored or
suppressed input. Automation counts are not yet limited; tenant quotas are a
production follow-up.

**Rejected:** Acknowledging before persistence loses accepted events on crash.
A separate asynchronous inbox processor is justified only when inbound work
becomes expensive or HTTP ingestion requires independently scalable buffering.

**Consequence:** The API needs PostgreSQL to accept events and may return a
retryable error during database outages. The existing upstream receiver is
responsible for redelivery.

## 4. Immutable execution snapshots

**Chosen:** Copy the matched automation configuration into each execution.

**Why:** A customer can disable a definition without changing a message sequence
that has already started. Snapshotting also makes the exact historical
configuration inspectable. Definitions otherwise remain immutable; changing the
configuration means creating another automation.

**Rejected:** Reading live configuration at every action can change the link or
prompt midway through a conversation. A separate full version-history subsystem
is unnecessary for a single fixed workflow.

**Consequence:** Snapshots duplicate a small amount of configuration. Disabling
an automation stops future matches, and does not cancel active runs.

## 5. One active conversation per account and user

**Chosen:** Permit one active execution for each tenant/account/platform/user.
Use ascending creation time, then ID, if several enabled automations match a comment.
Persistently suppress further matching comments during an active execution.

**Why:** An ordinary social DM has no workflow correlation token. Allowing
simultaneous email collectors for the same person would make its intended
recipient unknowable. A database invariant and conversation-level serialization
make this policy safe across service instances.

**Rejected:** Choosing an arbitrary active execution risks sending the wrong
link. Resuming every active execution surprises the user and couples unrelated
automations. Queuing future conversations would make stale comments initiate
unexpected prompts later.

**Consequence:** Only one matching automation runs at a time for that person on
that connected account. A future product can support explicit conversation
tokens or ask the user to select a workflow before loosening this invariant.

## 6. Receipt-order correlation with early-email capture

**Chosen:** A DM can satisfy the currently active execution even before the
email request has finished delivery. Persist its first valid email, then wait
for successful initial delivery before scheduling the final link. DMs received
before any execution exists remain unmatched.

**Why:** This closes the race between receiving a prompt response and committing
the waiting state. Retrospectively searching unrelated historical DMs cannot
establish which comment caused an email response.

**Rejected:** Accepting messages only in the waiting state loses a legitimate
fast response. Attaching every prior valid email to the next comment risks
reusing stale personal information. Caller timestamps alone do not establish
causal ordering or authenticity.

**Consequence:** A platform that delivers a DM before the triggering comment
needs causal reconciliation at its existing integration boundary, or the user
must send a new response after the session starts. Redelivering the same message
does not change its original unmatched outcome. The service makes this limitation
explicit rather than silently guessing.

## 7. Durable sequential actions and adapter-scoped idempotency

**Chosen:** Persist each logical action with a stable identifier. Execute the
reply before the email request, and the final link only after a valid email and
successful prompt delivery. Update workflow state and schedule subsequent work
in the same acknowledgment transaction.

**Why:** This avoids sending the link prematurely, prevents duplicate logical
actions, and closes the dual write between a workflow transition and its next
side effect. Platform calls remain outside database transactions.

**Rejected:** Calling a platform inside an HTTP/database transaction holds locks
across unbounded network latency and still cannot atomically commit with a real
external provider. Marking delivery before calling can lose messages on crash.

**Consequence:** A crash after successful external delivery but before database
acknowledgment causes a retry. The simulator deduplicates stable action IDs; a
real connector can promise the same only if its provider supports it. Otherwise
delivery is at least once, and duplicates are possible.

## 8. Leases, bounded retries, and explicit terminal states

**Chosen:** Durable expiring claims use ownership tokens. Retry temporary and
ambiguous failures with finite exponential backoff and jitter; surface permanent
or exhausted errors as failed executions. Persist an email-wait deadline and
expire abandoned conversations.

The waiting deadline begins when prompt delivery is acknowledged, and expires
only the waiting state. Ingestion checks the deadline as well as background
maintenance. Recovery must also finalize an expired lease whose last allowed
attempt crashed, rather than leaving it excluded from all future claims.

**Why:** A process crash must not strand work forever, and an unavailable
provider must not cause infinite retry loops. Completion means final outbound
delivery was acknowledged, not merely scheduled. Lease tokens prevent late
workers from overwriting a newer owner's database results.

**Rejected:** Permanent claims strand work; unbounded retries hide terminal
failure; process-local timers disappear on restart. Leases alone cannot provide
exactly-once external delivery.

**Consequence:** Database fencing protects state, not an in-flight platform
request. Production connectors need appropriate request deadlines, provider
idempotency where available, documented retry classification, and operator tools
for inspecting failures before any manual replay.
