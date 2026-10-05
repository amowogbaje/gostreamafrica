# Architecture (Phase 0)

Browser → nginx (:8088) → { web (Next.js), api (Go), MinIO via /hls and /public }.
api and worker share Postgres, Redis and object storage. The worker is a separate process
so FFmpeg CPU/memory use cannot starve API requests.

Buckets: `media-raw` (private uploads), `media-hls` (private, served via signed URLs later),
`media-public` (anonymous read: posters).

cPanel is a front-end/demo target only. Nothing in this design depends on it.
