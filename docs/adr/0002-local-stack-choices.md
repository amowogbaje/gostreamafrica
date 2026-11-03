# ADR-0002: Local stack choices
Status: accepted
- Migrations: golang-migrate (SQL files, up/down), run through its Docker image.
- Local object store: see ADR-0014 (was MinIO, ADR-0003). Prod uses R2/S3.
- nginx stands in for the CDN: CORS, Range/206, cache headers by file type. No proxy cache yet.
- Health: /healthz = liveness (no deps), /readyz = TCP reachability of postgres, redis, s3.
- Compose reads `.env`; `make up` creates it from `.env.example`.
