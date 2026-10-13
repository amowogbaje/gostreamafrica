-- Accent-insensitive search (Yoruba/Igbo names carry diacritics). unaccent() is only STABLE,
-- so a generated column needs this IMMUTABLE wrapper. The dictionary is resolved via search_path.
CREATE EXTENSION IF NOT EXISTS unaccent;
CREATE FUNCTION app_unaccent(text) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
    AS $$ SELECT unaccent('unaccent', $1) $$;

CREATE TYPE title_type   AS ENUM ('movie', 'series');
CREATE TYPE title_status AS ENUM ('draft', 'processing', 'published', 'unpublished');
CREATE TYPE credit_role  AS ENUM ('cast', 'director', 'writer');

CREATE TABLE genres (
    id   integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug text NOT NULL UNIQUE,
    name text NOT NULL UNIQUE,
    CONSTRAINT genres_slug_valid CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$')
);
-- Reference data (not sample data): every environment needs these.
INSERT INTO genres (slug, name) VALUES
    ('drama', 'Drama'), ('comedy', 'Comedy'), ('thriller', 'Thriller'), ('romance', 'Romance'),
    ('action', 'Action'), ('epic-historical', 'Epic & Historical'), ('family', 'Family'),
    ('documentary', 'Documentary'), ('musical', 'Musical'), ('horror', 'Horror');

CREATE TABLE people (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    photo_url  text,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT people_name_len  CHECK (char_length(name) BETWEEN 1 AND 120),
    CONSTRAINT people_photo_len CHECK (photo_url IS NULL OR char_length(photo_url) <= 500)
);

CREATE TABLE titles (
    id                uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    type              title_type   NOT NULL,
    slug              text         NOT NULL UNIQUE,
    name              text         NOT NULL,
    synopsis          text,
    release_year      smallint,
    maturity_rating   text,
    runtime_minutes   smallint,    -- movies; for series the runtime lives on each episode
    original_language text,
    poster_url        text,
    backdrop_url      text,
    status            title_status NOT NULL DEFAULT 'draft',
    -- RESTRICT: a user who owns titles cannot be deleted; suspend the account instead.
    creator_id        uuid         NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    published_at      timestamptz,
    created_at        timestamptz  NOT NULL DEFAULT now(),
    updated_at        timestamptz  NOT NULL DEFAULT now(),
    search_vector     tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('simple', app_unaccent(name)), 'A') ||
        setweight(to_tsvector('simple', app_unaccent(coalesce(synopsis, ''))), 'B')
    ) STORED,
    -- Lets seasons and media point at "a title of type X" with a plain foreign key.
    CONSTRAINT titles_id_type_key UNIQUE (id, type),
    CONSTRAINT titles_slug_valid      CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND char_length(slug) <= 120),
    CONSTRAINT titles_name_len        CHECK (char_length(name) BETWEEN 1 AND 200),
    CONSTRAINT titles_synopsis_len    CHECK (synopsis IS NULL OR char_length(synopsis) <= 4000),
    CONSTRAINT titles_year_valid      CHECK (release_year IS NULL OR release_year BETWEEN 1900 AND 2100),
    CONSTRAINT titles_rating_valid    CHECK (maturity_rating IS NULL OR maturity_rating IN ('G', 'PG', '12', '12A', '15', '18')),
    CONSTRAINT titles_runtime_valid   CHECK (runtime_minutes IS NULL OR runtime_minutes BETWEEN 1 AND 1000),
    CONSTRAINT titles_language_valid  CHECK (original_language IS NULL OR original_language ~ '^[a-z]{2,3}$'),
    CONSTRAINT titles_poster_len      CHECK (poster_url IS NULL OR char_length(poster_url) <= 500),
    CONSTRAINT titles_backdrop_len    CHECK (backdrop_url IS NULL OR char_length(backdrop_url) <= 500),
    -- A published title always says when; unpublished titles keep their original date.
    CONSTRAINT titles_published_has_date CHECK (status <> 'published' OR published_at IS NOT NULL)
);
-- Browse by status, newest first: also serves "latest published" with status = 'published'.
CREATE INDEX titles_status_published_idx ON titles (status, published_at DESC, id);
CREATE INDEX titles_creator_idx          ON titles (creator_id, created_at DESC);
CREATE INDEX titles_search_idx           ON titles USING gin (search_vector);

CREATE TABLE title_genres (
    title_id uuid    NOT NULL REFERENCES titles (id) ON DELETE CASCADE,
    genre_id integer NOT NULL REFERENCES genres (id) ON DELETE RESTRICT,
    PRIMARY KEY (title_id, genre_id)
);
-- "All titles in genre X"
CREATE INDEX title_genres_genre_idx ON title_genres (genre_id, title_id);

CREATE TABLE title_people (
    title_id       uuid        NOT NULL REFERENCES titles (id) ON DELETE CASCADE,
    person_id      uuid        NOT NULL REFERENCES people (id) ON DELETE RESTRICT,
    role           credit_role NOT NULL,
    character_name text,
    billing_order  smallint    NOT NULL DEFAULT 0,
    PRIMARY KEY (title_id, person_id, role),
    -- Deterministic credit order per title and role.
    CONSTRAINT title_people_order_key UNIQUE (title_id, role, billing_order),
    CONSTRAINT title_people_order_valid CHECK (billing_order >= 0),
    CONSTRAINT title_people_character_only_cast CHECK (character_name IS NULL OR role = 'cast')
);
-- A person's filmography
CREATE INDEX title_people_person_idx ON title_people (person_id);
