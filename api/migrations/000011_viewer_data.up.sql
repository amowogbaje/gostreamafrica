CREATE TABLE watchlist (
    user_id  uuid        NOT NULL REFERENCES users (id)  ON DELETE CASCADE,
    title_id uuid        NOT NULL REFERENCES titles (id) ON DELETE CASCADE,
    added_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, title_id)
);
-- "My list", newest first
CREATE INDEX watchlist_user_added_idx ON watchlist (user_id, added_at DESC);

-- One row per user and playable: a movie (episode_id NULL) or an episode.
CREATE TABLE watch_progress (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          uuid        NOT NULL REFERENCES users (id)  ON DELETE CASCADE,
    title_id         uuid        NOT NULL REFERENCES titles (id) ON DELETE CASCADE,
    episode_id       uuid,
    position_seconds integer     NOT NULL DEFAULT 0,
    duration_seconds integer     NOT NULL,
    completed        boolean     NOT NULL DEFAULT false,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    -- With episode_id NULL this is skipped (MATCH SIMPLE); otherwise the episode must belong to title_id.
    CONSTRAINT watch_progress_episode_fk FOREIGN KEY (episode_id, title_id)
        REFERENCES episodes (id, title_id) ON DELETE CASCADE,
    CONSTRAINT watch_progress_position_valid CHECK (position_seconds >= 0),
    CONSTRAINT watch_progress_duration_valid CHECK (duration_seconds > 0),
    CONSTRAINT watch_progress_within_duration CHECK (position_seconds <= duration_seconds)
);
CREATE UNIQUE INDEX watch_progress_movie_key   ON watch_progress (user_id, title_id)   WHERE episode_id IS NULL;
CREATE UNIQUE INDEX watch_progress_episode_key ON watch_progress (user_id, episode_id) WHERE episode_id IS NOT NULL;
-- "Continue watching": unfinished items, most recent first
CREATE INDEX watch_progress_continue_idx ON watch_progress (user_id, updated_at DESC) WHERE NOT completed;
