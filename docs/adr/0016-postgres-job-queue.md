# ADR-0016: The transcode queue is the transcode_jobs table in PostgreSQL
Status: Accepted

## Context
Prompt 2.2 needs a queue with retries, backoff, a dead-letter state, and recovery from crashed workers. Redis Streams
(a consumer group per worker) was the alternative. Video jobs are few and long (minutes to hours), not thousands per second.

## Decision
Jobs live in `transcode_jobs` (created by the API in the same transaction that marks an upload `uploaded`). A worker claims
with `UPDATE ... WHERE id = (SELECT ... FOR UPDATE SKIP LOCKED LIMIT 1)`, holds a lease (`locked_until`, extended by a
heartbeat every third of the lease), and records the outcome in a transaction that also moves the upload and the media asset.
- **At-least-once:** a job whose lease expires goes back to `queued` (the worker reaps expired leases on every loop); every step is
  safe to repeat (idempotent output, see docs/transcoding.md).
- **Retries:** failures re-queue with exponential backoff (30 s, 1 min, 2 min... capped at 15 min), up to `max_attempts` (3).
- **Dead letter:** a job out of attempts becomes `failed`; its upload shows `rejected` with code `processing_failed`. Nothing else retries it.
- **Permanent refusals** (unsupported codec, too long...) fail at once with the reason shown to the creator; no retry.
- **Graceful shutdown:** on SIGTERM the worker stops claiming, lets running jobs finish within `WORKER_SHUTDOWN_GRACE_SECONDS`, then
  hands the rest back with `Release`, which does not count as an attempt.
- Workers need DATABASE_URL (a new dependency of the worker; pgx, same version as the API).

## Consequences
- No extra moving part and no job lost between "job created" and "message published": both are one transaction.
- Postgres polling (every 2 s when idle) is wasteful at scale. If job volume grows to thousands per minute, move to Redis Streams or LISTEN/NOTIFY
  behind the same `runner.Queue` interface; `transcode_jobs` stays the source of truth.
- The progress shown to creators (`stage`, `percent`) is written to the job row at most about once per percent.
