-- Single-use, expiring tokens for email verification and password reset (hash only).
CREATE TYPE token_purpose AS ENUM ('email_verification', 'password_reset');

CREATE TABLE one_time_tokens (
    id         uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid          NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    purpose    token_purpose NOT NULL,
    token_hash bytea         NOT NULL UNIQUE,
    created_at timestamptz   NOT NULL DEFAULT now(),
    expires_at timestamptz   NOT NULL,
    used_at    timestamptz,
    CONSTRAINT one_time_tokens_hash_len CHECK (octet_length(token_hash) = 32)
);
CREATE INDEX one_time_tokens_user_purpose_idx ON one_time_tokens (user_id, purpose) WHERE used_at IS NULL;
