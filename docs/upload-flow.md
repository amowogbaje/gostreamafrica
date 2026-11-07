# Creator upload flow (Phase 2.1)

Creators send video straight to object storage in resumable parts. The API issues short-lived signed URLs,
checks the result and queues a job. It never carries the video bytes.

## Try it
```
make up && make migrate-up
# 1. register an account (Swagger UI at /api/docs/ works), then:
make dev-creator EMAIL=you@example.com        # prints the title_id of an empty draft movie
# 2. make a 20 s sample video (any .mp4/.mov/.mkv/.webm works):
ffmpeg -f lavfi -i testsrc=duration=20:size=1280x720:rate=30 -f lavfi -i sine=duration=20 -c:v libx264 -c:a aac sample.mp4
# 3. upload it
make upload-test ARGS="--email you@example.com --password '...' --title <title_id> --file sample.mp4"
# pause and resume
make upload-test ARGS="... --file sample.mp4 --stop-after 1"     # prints the upload id
make upload-test ARGS="... --file sample.mp4 --resume <upload_id>"
```
Use a file over 64 MiB to see several parts. The worker then processes it (`make logs s=worker`); the upload moves to `ready` and `GET .../uploads/{id}` shows the job's stage and percent (docs/transcoding.md).

## Endpoints (creator or admin role; full schema in docs/openapi.yaml)
| Call | Purpose |
|---|---|
| `POST /v1/creator/uploads` | start; returns `part_size_bytes`, `part_count`, URLs for the first 4 parts |
| `POST /v1/creator/uploads/{id}/parts` | fresh URLs for up to 20 chosen parts (URLs last 15 minutes) |
| `GET  /v1/creator/uploads/{id}` | state; while unfinished also `stored_parts`, which is how a client resumes |
| `POST /v1/creator/uploads/{id}/complete` | check, assemble, queue the job (202; safe to repeat) |
| `DELETE /v1/creator/uploads/{id}` | cancel an unfinished upload |
| `GET  /v1/creator/uploads/{id}/events` | live progress as Server-Sent Events (`status` events, then `end`); polling the GET above works too |
| `POST /v1/creator/uploads/{id}/retry` | give a dead-lettered upload (`processing_failed`) one more go, without uploading again |
| `POST /v1/creator/assets/{assetId}/unpublish` | hide the video's title from viewers; the files stay |
| `DELETE /v1/creator/assets/{assetId}` | delete the video for good: unpublish, remove HLS output, poster, thumbnails and the original, then the records |

Every part is `part_size_bytes` except the last (the remainder). The PUT body must be exactly the `size_bytes`
given for that part: the length is part of the signature, so storage refuses anything else.

## Life of an upload
`initiated` -> `uploaded` (job queued) -> `processing` -> `ready`, or `rejected` / `aborted` / `expired`; a finished video that its creator deletes becomes `deleted`.
One unfinished upload per title. Limits (env, see .env.example): 8 GiB per file, 3 unfinished uploads and 32 GiB per
24 h per creator, 10 starts per hour. An upload not completed within 24 h is aborted by a sweep job (hourly, one API
replica at a time via a Postgres advisory lock); it also aborts storage uploads that have no database row.

What `/complete` checks (in the API, no FFmpeg): all parts present with exact sizes, final size, the first bytes match
the claimed container (MP4/MOV `ftyp`, MKV/WebM EBML), virus scan (`creators.Scanner`; the local default
does nothing and logs a warning). A refusal ends the upload with a `reject_code`: `size_mismatch`,
`unsupported_container`, `container_mismatch`, `malware_detected`. Scanner outages fail closed (503, retry).

## Contract with the worker (implemented in Prompt 2.2: docs/transcoding.md, ADR-0016)
The queue is the `transcode_jobs` table in Postgres. A worker claims a job with:
```sql
UPDATE transcode_jobs SET state='running', locked_by=$1, locked_until=now()+interval '5 minutes',
       attempts=attempts+1, started_at=COALESCE(started_at, now()), updated_at=now()
 WHERE id = (SELECT id FROM transcode_jobs WHERE state='queued' AND run_after <= now()
             ORDER BY run_after FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING id, upload_id, policy;
```
- Source file: bucket/key from `uploads` (`bucket`, `object_key`).
- `policy` (JSON, copied at upload time): `containers`, `video_codecs`, `audio_codecs`, `min_duration_seconds`,
  `max_duration_seconds`, `max_width`, `max_height`. The worker runs ffprobe and checks the file against it.
- To refuse a file (not retryable): set the job `failed` with `finished_at`, and the upload
  `status='rejected'`, `reject_code` (e.g. `unsupported_codec`, `too_long`, `resolution_too_large`, `corrupt_file`),
  `reject_reason` (shown to the creator), in one transaction.
- Progress: write `stage` and `percent` on the job; `uploads.status` becomes `processing` on claim and `ready` on success.
- Retries: set `state='queued'`, `run_after` to a backoff time, keep `last_error`; stop at `max_attempts`.
  A job whose `locked_until` passed while `running` is dead and may be re-queued.

## Known limits
- Movies only; episodes come with the series workflow. A title with a video must have it removed before replacing (Phase 5).
- A creator can still occupy storage up to their daily quota before `/complete` notices. Parts are size-pinned, so
  the total cannot exceed what was declared.
- `ClamAV` or another real scanner is not wired yet (the interface is).
