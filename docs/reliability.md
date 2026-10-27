# Reliability and concurrency

The service coordinates a fixed email-collection workflow through PostgreSQL. Database transactions make incoming events, workflow state changes, and outgoing action creation atomic. A platform request is outside that transaction, so database atomicity does not imply exactly-once delivery to a social network.

This document describes the implemented guarantees and their boundaries.
[Testing](testing.md) records coverage, executed checks, and verification limits.

## Three independent kinds of state

- An **incoming event** records whether the service has already accepted a provider event and the result of processing it. A duplicate event does not create another logical transition.
- A **workflow execution** records progress through the conversation. Waiting is a database state; it consumes no worker, request, or sleeping goroutine.
- An **outgoing action** records one ordered platform operation, its attempts, retry time, and delivery result. A queued final message is not a completed workflow.

An execution completes only after the final action's successful result is committed. If the platform accepted a request but its response or the following database update was lost, the execution can remain pending even though the recipient already saw the message.

## Incoming delivery and replay

The HTTP receiver acknowledges an event only after committing its processing transaction. If that transaction fails, the caller receives an error and can retry. If a response is lost after commit, replaying the event is safe.

Deduplication is scoped by tenant, connected account, and event kind. Provider
event identifiers and underlying comment/message identifiers prevent a retried
platform object from producing additional logical work. Alternate event IDs for
the same object become receipt aliases. Reusing any known identity for changed
semantic content is a conflict, not an instruction to edit processed history.
The semantic hash excludes the envelope event ID. Inbox records retain identity,
hash, and outcome rather than raw DM text; they cannot replay raw message bodies.

Events that do not match an automation or active conversation are consumed with an ignored outcome. They do not become eligible when an automation or conversation is created later. In particular, a message received before its triggering comment is processed is not retrospectively attached to a new conversation. This prevents an old email from completing an unrelated future execution.

Concurrent requests are serialized at the conversation boundary. Database lock order, rather than client request-start time, defines the order of overlapping transactions. Provider delivery order is not treated as proof of the order in which a user performed actions.

## Conversation correlation

A conversation is identified by connected account and social user; the connected account establishes tenant ownership and platform. There is at most one active email-collection execution for that conversation across all automations. Database uniqueness enforces this invariant in addition to application locking.

When several enabled automations match the same comment, the oldest automation wins, with its identifier as a deterministic tie-breaker. Another comment while the conversation is active is durably consumed and suppressed. It is not delayed until the active execution finishes.

A valid email arriving after execution creation can be captured while initial actions are still pending. The final message remains ordered after the comment reply and initial DM. Capturing an early email and acknowledging an outgoing action use the same conversation lock so neither operation overwrites the other's progress. Invalid messages leave the execution active and do not queue a clarification message.

Correlation follows receipt order, not causal reply identity. If a response to an
older completed or expired conversation is delivered for the first time while a
new execution for that same account and user is active, it is treated as a
response to the current execution. The service cannot distinguish that delayed
reply without provider thread/reply identifiers or an additional event-time
contract. Previously consumed messages remain deduplicated and are never
retrospectively reassigned.

Automation configuration is copied into an execution when it starts. Definitions
are immutable except for their enabled flag. Disabling affects future triggers;
an active execution continues using its snapshot.

## Outgoing actions and workers

The transaction that changes workflow state also inserts the required outgoing
action. This avoids losing an action between updating the execution and
publishing to a separate queue. Uniqueness on execution and action kind prevents
duplicate logical actions in this fixed workflow.

Workers claim eligible actions through PostgreSQL row locks with `SKIP LOCKED`. Claiming creates a finite lease and a fresh ownership token, then commits. No transaction or conversation lock is held during platform I/O. An action becomes eligible only after its predecessor was acknowledged successfully.

Completion uses the lease token as a conditional update. A worker whose lease was replaced cannot mark the current attempt successful or failed. Expired leases are recoverable by another worker, including after a process crash. Recovery consumes another attempt; repeatedly crashing work does not retry forever.

**Lease fencing protects database state, not the remote platform.** A process can pause after checking its lease and resume after another worker claimed the action. Both can call the platform. Network timeouts shorter than the lease reduce this overlap but cannot eliminate it.

Every attempt reuses the same stable action identifier as the adapter idempotency key. The local simulated adapter persists a unique receipt for that identifier, so repeated calls produce one observable simulated effect. This is an adapter capability. A real adapter must establish whether its provider supports comparable idempotency and how long keys remain valid.

## Retries, failures, and expiry

Temporary errors and ambiguous timeouts are retried with bounded exponential backoff and jitter. The next attempt time and attempt count are persisted. Permanent errors or exhausted attempts fail the action and execution; subsequent actions cannot run. No automated retry resets a terminal failure.

An ambiguous timeout is treated as potentially successful remotely. With an idempotent adapter, retrying the same action is safe. Without that capability, retrying may duplicate a visible message. Even a terminal failed execution can have produced a remote effect before its response was lost. An operator must inspect the provider before manually re-running such work.

Waiting has a persisted deadline. Expiry ends a conversation that never supplies an email and frees its active-conversation slot. Expiry applies to the email-waiting phase; it must not overwrite a final action's committed completion. Expired and failed executions remain inspectable rather than disappearing silently.

Expiry transitions triggered by event ingestion and by the periodic sweep are
logged after commit. A rolled-back transition does not emit a successful state
change log.

## Crash boundaries

| Failure point | Durable result and recovery |
| --- | --- |
| Before incoming transaction commit | No partial workflow or action; caller retries. |
| After incoming commit, before HTTP response | Replay finds the committed event; no duplicate logical execution. |
| After action creation, before worker claim | Another poll finds the pending action. |
| After claim, before platform call | Lease expires; a worker retries with the same action identifier. |
| After platform success, before acknowledgment commit | Same action is retried; simulated receipts deduplicate it. Real-provider outcome can be ambiguous. |
| During workflow advancement | Transaction rollback preserves the preceding state and action result together. |
| While waiting for email | Restart reads the active execution from PostgreSQL. |
| After final acknowledgment commit | Execution is completed and the final action is no longer eligible. |

## Operation and scaling

Multiple service instances may share the same database and process queue. Conversation transactions serialize only related users; workers skip rows already locked by another claimant. A message broker is unnecessary for this workload because the workflow and queue need the same transactional storage.

Structured logs identify event, execution, action, attempt, and safe error
classification while omitting email addresses and DM bodies. Readiness depends on
PostgreSQL. Shutdown drains HTTP requests within its deadline, then cancels
workers through their contexts; any abandoned lease remains recoverable.

Operators should investigate failed actions, unusually old pending actions, expired leases, and growing inbox/outbox tables. Production follow-ups include monitored queue age and failure rates, provider-specific rate limiting and retry hints, an audited redrive procedure, and retention policies that remove message bodies and collected email addresses without prematurely removing deduplication identities.

## Accepted boundaries

- Real social-network delivery cannot be called exactly once without provider guarantees.
- A DM committed before any active execution is ignored rather than replayed into a later session.
- Only one active conversation is supported per connected account and social user. Parallel email requests require explicit reply/thread correlation before relaxing this rule.
- PostgreSQL availability is required to accept an event or claim work.
- The local simulated adapter validates engine behavior; it does not validate actual Instagram API permissions, limits, or delivery behavior.
- There is no claim of arbitrarily scalable throughput. Database contention and queue volume should be measured before introducing another coordination service.
