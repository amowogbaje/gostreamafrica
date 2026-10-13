-- Seasons exist only for series. The composite foreign key (title_id, title_type) to
-- titles (id, type) makes Postgres reject a season on a movie, and block changing a
-- title's type while it still has seasons.
CREATE TABLE seasons (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    title_id   uuid        NOT NULL,
    title_type title_type  NOT NULL DEFAULT 'series',
    number     smallint    NOT NULL,
    name       text,
    synopsis   text,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT seasons_title_fk FOREIGN KEY (title_id, title_type)
        REFERENCES titles (id, type) ON DELETE CASCADE,
    CONSTRAINT seasons_only_series  CHECK (title_type = 'series'),
    CONSTRAINT seasons_number_valid CHECK (number > 0),
    CONSTRAINT seasons_name_len     CHECK (name IS NULL OR char_length(name) <= 200),
    CONSTRAINT seasons_title_number_key UNIQUE (title_id, number),
    CONSTRAINT seasons_id_title_key     UNIQUE (id, title_id)
);

-- Episodes repeat title_id so progress rows can prove an episode belongs to the title.
CREATE TABLE episodes (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    season_id       uuid        NOT NULL,
    title_id        uuid        NOT NULL,
    number          smallint    NOT NULL,
    name            text        NOT NULL,
    synopsis        text,
    runtime_minutes smallint,
    air_date        date,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT episodes_season_fk FOREIGN KEY (season_id, title_id)
        REFERENCES seasons (id, title_id) ON DELETE CASCADE,
    CONSTRAINT episodes_number_valid  CHECK (number > 0),
    CONSTRAINT episodes_name_len      CHECK (char_length(name) BETWEEN 1 AND 200),
    CONSTRAINT episodes_runtime_valid CHECK (runtime_minutes IS NULL OR runtime_minutes BETWEEN 1 AND 1000),
    CONSTRAINT episodes_season_number_key UNIQUE (season_id, number),
    CONSTRAINT episodes_id_title_key      UNIQUE (id, title_id)
);
CREATE INDEX episodes_title_idx ON episodes (title_id);
