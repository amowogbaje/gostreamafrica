CREATE TABLE users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    email         citext      NOT NULL UNIQUE,
    password_hash text        NOT NULL,
    name          text        NOT NULL,
    role          text        NOT NULL DEFAULT 'viewer',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_email_len CHECK (char_length(email::text) <= 254),
    CONSTRAINT users_name_len  CHECK (char_length(name) BETWEEN 1 AND 80),
    CONSTRAINT users_role_valid CHECK (role IN ('viewer', 'creator', 'admin'))
);
