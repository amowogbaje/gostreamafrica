# Transcoding worker (Phase 2.2)

The worker turns an uploaded source video into adaptive-bitrate HLS plus a poster and thumbnails. One process claims jobs from
the `transcode_jobs` table (ADR-0016), runs FFmpeg, and publishes to object storage.

## Pipeline (progress is the number the creator sees)
| Stage | % | What happens |
|---|---|---|
| download | 2 | source copied from `media-raw/{uploadId}/source.ext` to scratch disk (a free-space check comes first) |
| probe | 12 | ffprobe, then the limits copied into the job are applied. A refusal is final: `unsupported_codec`, `too_long`, `too_short`, `resolution_too_large`, `corrupt_file`, `no_video_stream`, `unsupported_container` |
| transcode | 12 to 85 | one FFmpeg process decodes once and writes every rung |
| extras | 85 to 91 | poster, thumbnail sprite + WebVTT index, subtitles (.srt to .vtt). Failures here skip the extra, not the video |
| upload | 92 to 99 | stale output cleared, media files uploaded, then playlists, the master last |
| done | 100 | one transaction: job succeeded, upload ready, asset ready with manifest key and duration |

## The ladder (`internal/transcode/ladder.go`)
| Rung | Video | Audio | About |
|---|---|---|---|
| 240p | 250 kbps | 48 kbps | data saver, 300 kbps all in |
| 360p | 600 kbps | 64 kbps | default on weak networks |
| 480p | 1000 kbps | 96 kbps | about 450 MB per hour |
| 720p | 2200 kbps | 128 kbps | about 1 GB per hour |
| 1080p | 4200 kbps | 128 kbps | |
Rungs above the source's SHORT side are skipped (never upscale; a portrait phone video is laddered by its width). A source smaller
than 240p gets one rung at its own size. Sources above 30 fps are capped to 30.

## FFmpeg choices (`BuildHLSArgs`)
- **H.264 main profile, yuv420p, AAC stereo 48 kHz:** plays on every phone and browser.
- **Constrained bitrate** (`-b:v`, `-maxrate` 110 %, `-bufsize` 2x): predictable bandwidth for the master playlist and data plans.
- **Keyframes forced every 6 s** (`-force_key_frames`, `-sc_threshold 0`) so every 6 s segment starts with a keyframe and quality can switch cleanly
  (`independent_segments`).
- **fMP4 HLS** (`-hls_segment_type fmp4`): one `init_N.mp4` plus `seg_%05d.m4s` per rung, `#EXT-X-PLAYLIST-TYPE:VOD`; `master.m3u8` is written by FFmpeg
  with correct RESOLUTION and CODECS.
- **Loudness normalised** to -16 LUFS (`loudnorm`), so quiet and loud films play at a similar volume on a phone speaker.
- **`veryfast` preset** by default (`WORKER_X264_PRESET`); slower presets give smaller files for more CPU time.

## Storage layout
- `media-hls/{assetId}/master.m3u8`, `{rung}/index.m3u8`, `{rung}/init_N.mp4`, `{rung}/seg_00000.m4s`..., `subtitles/{lang}.vtt` (private)
- `media-public/{titleId}/poster.jpg`, `thumbs.jpg`, `thumbs.vtt` (public, cached for a year)
- Playlists are cached 5 minutes; segments, init files and images are `immutable`.
Subtitles: put `media-raw/{uploadId}/subtitles/{language}.srt` (for example `en.srt`, `pt-BR.srt`) before the job runs. UTF-8 only; a bad file is skipped and logged.

## Failure handling
| Situation | What happens |
|---|---|
| File not acceptable | job `failed`, upload `rejected` + `reject_code`/`reject_reason`, asset `failed`; no retry |
| FFmpeg crash, storage or database hiccup, timeout, no disk space | retry after 30 s, 1 min, 2 min... (max 15 min), 3 attempts, then dead letter (upload `rejected`, `processing_failed`) |
| Worker killed or hung | lease (2 min) expires, any worker re-queues it (`Reap`) |
| Worker told to stop | stops claiming, running jobs get 60 s, the rest are released without losing an attempt |
| Re-run of a job | same asset id; old output under `{assetId}/` is deleted before upload; media first, playlists last |
FFmpeg is started with `Pdeathsig` on Linux, so it dies with the worker. Scratch directories (`WORKER_TEMP_DIR`) are removed after each job and stale ones at start.

## Settings (env, see .env.example)
`WORKER_CONCURRENCY` (1), `WORKER_FFMPEG_THREADS` (2), `WORKER_X264_PRESET` (veryfast), `WORKER_JOB_TIMEOUT_MINUTES` (240), `WORKER_LEASE_SECONDS` (120),
`WORKER_POLL_SECONDS` (2), `WORKER_BACKOFF_BASE_SECONDS` (30), `WORKER_BACKOFF_MAX_MINUTES` (15), `WORKER_SHUTDOWN_GRACE_SECONDS` (60), `WORKER_MIN_FREE_MB` (1024).

## Trying it
```
make up && make migrate-up
make dev-creator EMAIL=you@example.com            # prints a title id
make upload-test ARGS="--email you@example.com --password '...' --title <id> --file sample.mp4"
make logs s=worker                                 # job started, probed, transcoded, published
```
The creator's `GET /v1/creator/uploads/{id}` shows `processing` with `job.stage` and `job.percent` while it runs, then `ready`.
Not done yet: a preview clip, 4K output, per-title encoding (all later), and the playback endpoints (Prompt 2.3).
