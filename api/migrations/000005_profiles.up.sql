-- One profile per user. Deleting a user deletes the profile.
CREATE TABLE profiles (
    user_id            uuid        PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    display_name       text        NOT NULL,
    avatar_url         text,
    country            char(2),
    preferred_language text        NOT NULL DEFAULT 'en',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT profiles_display_name_len CHECK (char_length(display_name) BETWEEN 1 AND 80),
    CONSTRAINT profiles_avatar_url_valid CHECK (avatar_url IS NULL OR (char_length(avatar_url) <= 500 AND avatar_url ~ '^https://')),
    CONSTRAINT profiles_country_valid    CHECK (country IS NULL OR country ~ '^[A-Z]{2}$'),
    CONSTRAINT profiles_language_valid   CHECK (preferred_language ~ '^[a-z]{2,3}(-[A-Z]{2})?$')
);

-- The display name moves from users.name to profiles. Existing rows are preserved.
INSERT INTO profiles (user_id, display_name) SELECT id, name FROM users;
ALTER TABLE users DROP COLUMN name;
