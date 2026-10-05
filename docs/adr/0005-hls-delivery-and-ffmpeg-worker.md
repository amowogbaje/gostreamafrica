# ADR-0005: HLS delivery with a separate FFmpeg worker
Status: Accepted

## Context
Viewers are mostly on mobile data in Nigeria and across Africa, so bandwidth varies a lot and playback must adapt. FFmpeg is CPU and memory heavy and runs for minutes, so it must not compete with API requests. cPanel cannot run it.

## Decision
- **Upload:** creators upload straight to the private `media-raw` bucket with presigned multipart uploads. The API never proxies video bytes.
- **Jobs:** the API writes a `transcode_job` row in Postgres (queued, running, succeeded, failed, attempts) and pushes its id onto a Redis queue. Postgres is the source of truth and Redis only wakes workers. A reconciler re-enqueues queued jobs if Redis is lost.
- **Worker:** a separate process claims a job (`FOR UPDATE SKIP LOCKED`), downloads the source, runs FFmpeg, uploads output to `media-hls`, and retries with backoff up to a maximum. Failed jobs stay visible.
- **Output:** H.264 video and AAC audio, an adaptive ladder of 240p, 360p, 480p and 720p (starting bitrates to be tuned on real films), 6 second keyframe-aligned segments, MPEG-TS for the widest player support, a master playlist, and a poster image. No 1080p or 4K in the MVP.
- **Idempotency:** output goes under `hls/<title_id>/<job_id>/`. The title's active version pointer flips only after every file uploads successfully.

## Consequences
- The API stays responsive during transcodes, and workers scale or crash independently.
- We must build and test a small job state machine plus reconciliation.
- Storage grows by roughly the sum of the ladder per film, and the 240p rung keeps data use low.
- At launch the worker may share a VPS with the API, so limit it to one concurrent job and lower priority. Move it to its own host when queue times hurt.
- fMP4 and a 1080p rung can be added later without changing the flow.
