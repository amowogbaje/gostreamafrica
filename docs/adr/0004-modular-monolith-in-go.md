# ADR-0004: Modular monolith in Go instead of microservices
Status: Accepted

## Context
A small team must prove one loop: upload, process, discover, watch, pay. There are six areas (auth, catalog, playback, billing, creators, analytics), one production target (a cloud VPS with managed Postgres) and no load data yet. The money path, payment confirmed then viewer entitled, must stay transactionally simple. Microservices would add network failure modes, per-service deploy and monitoring, and distributed transactions before there is any evidence we need them.

## Decision
- One Go binary (`api`) and one PostgreSQL database. Code is split into modules under `api/internal/<module>`.
- Boundary rules: a module exposes a small exported interface. Other modules call that interface and never import its internals or query its tables. Shared plumbing lives only in `internal/platform`.
- Boundaries are enforced in CI with a `depguard` lint rule, added when the first cross-module call appears.
- The FFmpeg worker is the one separate process, because its workload is different (ADR-0005).
- A module becomes its own service only on measured evidence: it must scale independently, fail independently, or be owned by a separate team.

## Consequences
- One deploy, one log stream, in-process calls and a single DB transaction for payment to entitlement. Easy to run with `docker compose up`.
- The stateless API scales by adding replicas behind the proxy, so growth does not force a split.
- A bad deploy or memory leak affects every module. Mitigation: health checks, fast rollback, race-detector tests.
- Boundaries are convention, not compiler guarantees, so they erode without the lint rule.
- Revisit when one module needs a different scaling profile or release cadence.
