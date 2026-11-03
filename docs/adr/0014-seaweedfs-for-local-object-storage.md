# ADR-0014: SeaweedFS replaces MinIO for local object storage
Status: Accepted (supersedes ADR-0003)

## Context
MinIO's community images were removed (ADR-0003), leaving local development on an unpatched third-party mirror. Phase 2
(uploads, FFmpeg, HLS) is almost entirely object-storage work, so the store should be settled before it starts.
ADR-0006 already requires plain S3 only, so the swap is configuration.

## Decision
- Local store: **SeaweedFS** (`chrislusf/seaweedfs`, pinned in `SEAWEEDFS_IMAGE`), one container running `weed server -s3`
  (master + volume + filer + S3 gateway). Apache-2.0 licensed and actively released. Local development only; staging and
  production keep R2 or S3 (ADR-0006).
- Variables are vendor neutral: `S3_ENDPOINT`, `S3_PUBLIC_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_REGION`,
  `S3_BUCKET_*`, path-style addressing. No `MINIO_*` variables remain.
- **Auth is explicit.** SeaweedFS accepts any key when no config file is loaded, so `infra/seaweedfs/entrypoint.sh` renders an
  identities file from `.env`: `anonymous` may only read `media-public`; the application identity has full access.
- Only the S3 port is published. The filer (8888) and master (9333) ports accept unauthenticated writes, so they stay on the
  Docker network. `make storage-ls` replaces the MinIO console.
- Buckets are created over the S3 API by `storage-init` (curl SigV4, same image).
- Browser uploads use CORS set with `-s3.allowedOrigins` (`S3_ALLOWED_ORIGINS`, the web origins).
- Dev tuning in `infra/seaweedfs/master.toml`: one pre-created volume per bucket instead of seven (see Consequences).
- `make s3-check` proves the features Phase 2 needs against the running server: multipart (and abort), presigned URLs (PUT,
  GET, part upload, expiry, tampering, content-type enforcement), anonymous rules per bucket, byte ranges, CORS with ETag
  exposed, prefix listing with pagination, batch delete, metadata round-trip and default SDK checksums.

## Consequences
- **Volume exhaustion trap:** by default SeaweedFS pre-creates 7 volumes the first time each bucket is written. With three
  buckets and a volume cap of 20, uploads fail with "failed to find writable volumes". `master.toml` sets `copy_1 = 1`.
- **No lifecycle rules assumed.** The abandoned-multipart cleanup (Prompt 2.1) is an application job: list uploads, read
  `Initiated`, abort those older than 24 h. `make s3-check` proves the listing exposes `Initiated`.
- **Open item for Prompt 2.3:** nginx proxies HLS from the private `media-hls` bucket, and nginx cannot sign S3 requests.
  Decide then between a second unpublished S3 listener that nginx may read anonymously, or the API streaming signed redirects.
- Presigned URLs are signed for `S3_PUBLIC_ENDPOINT` (the host the browser uses), not the internal `seaweedfs:8333`.
- If a new AWS SDK sends checksum headers an S3-compatible store rejects, set request checksums to "when required". The
  current check shows the default behaviour is accepted.
- Production topology (separate master, volume and filer, replication) is not designed here. Production uses R2 or S3.
