# AI usage disclosure

I set the engineering direction through a detailed brief covering the
requirements, design constraints, and development workflow.
The brief established Go and PostgreSQL as the preferred stack and prioritized
durable execution, transactional consistency, tenant isolation, explicit failure
semantics, and a small, reviewable implementation. It also required architectural
tradeoffs to be explained and the complete scenario to be demonstrated.

The workflow I specified covered requirements and acceptance criteria,
architecture and failure analysis, incremental implementation, integration
testing, and separate reliability, security, and code reviews.

AI assistance was substantial. Within the constraints I supplied, AI tools helped
develop the detailed architecture, state machine, database schema, and
concurrency strategy, and produce the Go implementation, migrations, tests,
Docker/CI configuration, demo, and documentation.

The verification process included real PostgreSQL integration tests, the Go race
detector, and a reproducible HTTP demo covering restart recovery and ambiguous
delivery failures. The [testing notes](testing.md) document the checks
executed and their limits.
