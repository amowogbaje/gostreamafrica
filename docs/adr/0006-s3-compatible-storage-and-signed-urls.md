# ADR-0006: S3-compatible storage with signed URLs
Status: Accepted

## Context
Media must stay private until a viewer is entitled, and the same code must run locally and in the cloud. MinIO stopped publishing official images (ADR-0003), so the local store may change.

## Decision
- The application speaks the plain S3 API only. Endpoint, credentials, region, path-style flag and bucket names come from environment variables. No vendor-specific calls and no `if local` branches.
- Buckets: `media-raw` (private), `media-hls` (private) and `media-public` (anonymous read, posters only).
- Local: a MinIO-compatible store as pinned in ADR-0003, replaceable. Cloud: Cloudflare R2 (no egress fees), or AWS S3.
- **Uploads:** presigned PUT or multipart with a 15 minute expiry and size and content-type limits.
- **Playback:** the API checks entitlement, then returns a URL containing an HMAC token over the title's path prefix and an expiry. One signed file URL cannot cover an HLS playlist plus hundreds of segments, so the token covers the whole prefix. The CDN edge validates it: a Cloudflare Worker in the cloud, nginx `secure_link` locally (verify with `nginx -V`). Token lifetime is a few hours, set per environment.
- Segments are immutable and cached for a year. Playlists are cached for seconds.

## Consequences
- Moving between MinIO-like stores, R2 and S3 is configuration only.
- R2 and S3 differ in CORS rules, signing details and addressing style. A smoke test (upload, byte-range fetch, signed URL) must pass against a real R2 dev bucket before staging.
- The token check exists in two forms (nginx and Worker). Both must pass the same test vectors.
- A leaked link works until it expires. Accepted for the MVP, since DRM is out of scope.
- Origin buckets are never public, and the cloud origin is reachable only through the CDN.
