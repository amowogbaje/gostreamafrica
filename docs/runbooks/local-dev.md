# Local dev runbook
- Reset everything: `make clean && make up`
- Tail one service: `make logs s=api`
- API won't start: `docker compose logs api` (config errors list every missing variable)
- Open a DB shell: `docker compose exec postgres psql -U streamafrica`
- Check buckets and sizes: `make storage-ls`
- Prove the object store works (multipart, presigned URLs, CORS, ranges): `make s3-check`
- Changed `S3_ACCESS_KEY`/`S3_SECRET_KEY`: `docker compose up -d --force-recreate seaweedfs`

## Running the tests
- `make test` runs everything (API, worker, web) against the running stack, so start it first with `make up`. The database,
  Redis, object-storage and FFmpeg tests use throwaway schemas and test prefixes, never your data. With `REQUIRE_INTEGRATION=1`
  (set by `make test` and CI) a missing service is a **failure**, not a silent skip: if you see "may not be skipped", run `make up`.
- `make test-unit` runs only the tests that need no services (the stack can be down); the integration tests skip.
- The worker's most important test is `internal/pipeline` (real Postgres, real storage, real FFmpeg: upload, transcode, publish, re-run,
  refusals). It runs in `make test` and in CI (which starts SeaweedFS from compose).
