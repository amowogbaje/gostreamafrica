# ADR-0003: MinIO images are no longer published
Status: accepted (temporary)
- Symptom: `pull access denied for minio/minio, repository does not exist`.
- Cause: MinIO deleted its Docker Hub repositories (Sept 2026) and later gated quay.io.
- Local fix: `MINIO_IMAGE` / `MC_IMAGE` env vars point at unmodified third-party mirrors of the
  last community release (scnd/minio-mirror, scnd/mc-mirror). amd64 only, hence `platform: linux/amd64`.
- Risks: the last free release has a known auth-bypass CVE and a mirror can vanish. Acceptable for
  local dev bound to 127.0.0.1. NEVER use it in staging or production.
- Rule going forward: application code speaks plain S3 (endpoint, keys, bucket via env), never MinIO-specific APIs.
  Production uses Cloudflare R2 or S3, so nothing here leaks into prod.
- Open decision: replace the local store with SeaweedFS or Garage. That changes the `minio`
  and `minio-init` services only. Needs your approval because the stack is fixed.
- Make it durable now: after the first successful pull, `docker save` the two images or push them to your own registry.
