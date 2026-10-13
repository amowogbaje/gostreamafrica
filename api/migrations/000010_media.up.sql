CREATE TYPE asset_status AS ENUM ('processing', 'ready', 'failed');

-- A playable HLS asset: one per movie title or one per episode (exactly one of the two ids).
-- That the title is a movie is enforced by the catalog module, not by this table.
CREATE TABLE media_assets (
    id               uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    title_id         uuid         REFERENCES titles (id) ON DELETE CASCADE,
    episode_id       uuid         REFERENCES episodes (id) ON DELETE CASCADE,
    status           asset_status NOT NULL DEFAULT 'processing',
    source_key       text,
    hls_manifest_key text,
    duration_seconds integer,
    created_at       timestamptz  NOT NULL DEFAULT now(),
    updated_at       timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT media_assets_one_target    CHECK (num_nonnulls(title_id, episode_id) = 1),
    CONSTRAINT media_assets_duration      CHECK (duration_seconds IS NULL OR duration_seconds > 0),
    CONSTRAINT media_assets_ready_has_hls CHECK (status <> 'ready' OR (hls_manifest_key IS NOT NULL AND duration_seconds IS NOT NULL))
);
CREATE UNIQUE INDEX media_assets_title_key   ON media_assets (title_id)   WHERE title_id IS NOT NULL;
CREATE UNIQUE INDEX media_assets_episode_key ON media_assets (episode_id) WHERE episode_id IS NOT NULL;

CREATE TABLE subtitles (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_id   uuid        NOT NULL REFERENCES media_assets (id) ON DELETE CASCADE,
    language   text        NOT NULL,
    label      text        NOT NULL,
    vtt_url    text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT subtitles_language_valid CHECK (language ~ '^[a-z]{2,3}(-[A-Z]{2})?$'),
    CONSTRAINT subtitles_label_len      CHECK (char_length(label) BETWEEN 1 AND 60),
    CONSTRAINT subtitles_url_len        CHECK (char_length(vtt_url) BETWEEN 1 AND 500),
    CONSTRAINT subtitles_asset_language_key UNIQUE (asset_id, language)
);
