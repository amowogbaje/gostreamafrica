-- Phase 2.1: creator uploads and the transcode job queue.
-- No updated_at trigger: like the rest of the schema, application code sets updated_at.

CREATE TYPE upload_status AS ENUM (
    'initiated',   -- multipart upload started; the creator is sending parts
    'uploaded',    -- all parts stored and checked; a transcode job is queued
    'processing',  -- the worker has the job
    'ready',       -- the worker finished: an HLS asset exists
    'rejected',    -- the file is not acceptable (see reject_code / reject_reason)
    'aborted',     -- the creator cancelled
    'expired'      -- never finished within the allowed time; storage was cleaned up
);

CREATE TYPE job_state AS ENUM ('queued', 'running', 'succeeded', 'failed', 'canceled');

-- One row per attempt to upload a source video for a movie title.
-- Bytes never pass through the API: the creator sends them straight to object storage.
CREATE TABLE uploads (
    id              uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    creator_id      uuid          NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    title_id        uuid          NOT NULL REFERENCES titles (id) ON DELETE CASCADE,
    filename        text          NOT NULL,
    content_type    text          NOT NULL,
    container       text          NOT NULL,           -- mp4, mov, mkv or webm (from the file extension)
    size_bytes      bigint        NOT NULL,
    part_size_bytes bigint        NOT NULL,
    part_count      integer       NOT NULL,
    bucket          text          NOT NULL,
    object_key      text          NOT NULL,
    s3_upload_id    text,                             -- cleared once the multipart upload is completed or aborted
    status          upload_status NOT NULL DEFAULT 'initiated',
    reject_code     text,
    reject_reason   text,
    probe           jsonb,                            -- ffprobe summary, written by the worker
    expires_at      timestamptz   NOT NULL,           -- an upload still 'initiated' after this is swept
    completed_at    timestamptz,
    created_at      timestamptz   NOT NULL DEFAULT now(),
    updated_at      timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT uploads_size_valid      CHECK (size_bytes > 0),
    CONSTRAINT uploads_part_valid      CHECK (part_size_bytes > 0 AND part_count BETWEEN 1 AND 10000),
    CONSTRAINT uploads_filename_len    CHECK (char_length(filename) BETWEEN 1 AND 255),
    CONSTRAINT uploads_container_valid CHECK (container IN ('mp4', 'mov', 'mkv', 'webm')),
    CONSTRAINT uploads_rejected_reason CHECK (status <> 'rejected' OR reject_code IS NOT NULL)
);
CREATE INDEX uploads_creator_idx ON uploads (creator_id, created_at DESC);
-- The sweep looks for unfinished uploads past their deadline.
CREATE INDEX uploads_sweep_idx ON uploads (expires_at) WHERE status = 'initiated';
-- At most one unfinished upload per title: a second one must wait or cancel the first.
CREATE UNIQUE INDEX uploads_one_active_per_title ON uploads (title_id)
    WHERE status IN ('initiated', 'uploaded', 'processing');

-- The queue the worker reads. Created in the same transaction that marks the upload 'uploaded'.
CREATE TABLE transcode_jobs (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    upload_id    uuid        NOT NULL REFERENCES uploads (id) ON DELETE CASCADE,
    state        job_state   NOT NULL DEFAULT 'queued',
    attempts     integer     NOT NULL DEFAULT 0,
    max_attempts integer     NOT NULL DEFAULT 3,
    run_after    timestamptz NOT NULL DEFAULT now(),  -- backoff: not claimable before this
    locked_by    text,                                -- worker instance holding the lease
    locked_until timestamptz,                         -- lease expiry; a crashed worker's job becomes claimable again
    stage        text,                                -- probe, transcode, upload...: shown to the creator
    percent      smallint,
    policy       jsonb       NOT NULL,                -- validation limits as they were when the job was created
    last_error   text,
    started_at   timestamptz,
    finished_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT transcode_jobs_attempts_valid CHECK (attempts >= 0 AND max_attempts >= 1),
    CONSTRAINT transcode_jobs_percent_valid  CHECK (percent IS NULL OR percent BETWEEN 0 AND 100),
    CONSTRAINT transcode_jobs_finished_set   CHECK (state NOT IN ('succeeded', 'failed', 'canceled') OR finished_at IS NOT NULL)
);
-- Workers claim with SELECT ... FOR UPDATE SKIP LOCKED over this index.
CREATE INDEX transcode_jobs_claim_idx ON transcode_jobs (run_after) WHERE state = 'queued';
CREATE INDEX transcode_jobs_upload_idx ON transcode_jobs (upload_id);
-- A running or queued job per upload at most.
CREATE UNIQUE INDEX transcode_jobs_one_open_per_upload ON transcode_jobs (upload_id)
    WHERE state IN ('queued', 'running');
