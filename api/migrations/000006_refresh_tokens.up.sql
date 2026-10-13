-- Opaque server-side sessions are replaced by JWT access tokens + rotating refresh tokens.
DROP TABLE sessions;

-- Only the SHA-256 of a refresh token is stored. Tokens rotate on every use and belong to a
-- family: presenting an already-used token revokes the whole family.
CREATE TABLE refresh_tokens (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id         uuid        NOT NULL DEFAULT gen_random_uuid(),
    token_hash        bytea       NOT NULL UNIQUE,
    created_at        timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL,
    family_expires_at timestamptz NOT NULL,
    used_at           timestamptz,
    revoked_at        timestamptz,
    replaced_by       uuid        REFERENCES refresh_tokens (id) ON DELETE SET NULL,
    user_agent        text,
    ip                inet,
    CONSTRAINT refresh_tokens_hash_len   CHECK (octet_length(token_hash) = 32),
    CONSTRAINT refresh_tokens_ua_len     CHECK (user_agent IS NULL OR char_length(user_agent) <= 200),
    CONSTRAINT refresh_tokens_expiry_cap CHECK (expires_at <= family_expires_at)
);
CREATE INDEX refresh_tokens_user_active_idx ON refresh_tokens (user_id) WHERE revoked_at IS NULL;
CREATE INDEX refresh_tokens_family_idx      ON refresh_tokens (family_id);
CREATE INDEX refresh_tokens_expires_idx     ON refresh_tokens (expires_at);
