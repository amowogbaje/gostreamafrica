# ADR-0003: MinIO community images are no longer published
Status: Superseded by ADR-0014 (local store is now SeaweedFS)

## Context
The pull error `pull access denied for minio/minio` is real: MinIO stopped publishing community images in October 2025,
put the community edition into maintenance mode in December 2025, archived the repository in February 2026 and deleted the
`minio/minio` and `minio/mc` repositories from Docker Hub on 11 September 2026. There is no newer official community release
to upgrade to. The last community image, RELEASE.2025-09-07T16-13-09Z, predates a vulnerability disclosed in October 2025
whose fix exists only as source. Maintained alternatives exist: Chainguard's free `minio` and `minio-client` images, and the
`pgsty/minio` fork.

## Decision
- Keep MinIO for local development. `MINIO_IMAGE` and `MC_IMAGE` in `.env` point at unmodified third-party mirrors of the last
  community release (scnd/minio-mirror, scnd/mc-mirror). They are amd64 only, hence `platform: linux/amd64`.
- Limits: never used in staging or production (those use R2 or S3), bound to 127.0.0.1, holds disposable data only.
- Application code speaks plain S3 only (ADR-0006), so the local store is replaceable by changing two image variables and,
  at most, the init script.
- Save the images once: `docker save` both, or push them to your own registry.

## Consequences
- Local development keeps working with no code changes.
- No security patches. A mirror can vanish. Revisit when real creator uploads are built, or when a mirror disappears.
- If swapping to a maintained MinIO build: Chainguard images are distroless (no shell, curl or wget), so the minio healthcheck
  and the `minio-init.sh` script need rework, and free-tier images are `latest` only, so pin by digest. Test with `make smoke`.
