CREATE TYPE user_role   AS ENUM ('viewer', 'creator', 'admin');
CREATE TYPE user_status AS ENUM ('active', 'suspended');

ALTER TABLE users DROP CONSTRAINT users_role_valid;
ALTER TABLE users ALTER COLUMN role DROP DEFAULT;
ALTER TABLE users ALTER COLUMN role TYPE user_role USING role::user_role;
ALTER TABLE users ALTER COLUMN role SET DEFAULT 'viewer';

ALTER TABLE users ADD COLUMN status user_status NOT NULL DEFAULT 'active';
ALTER TABLE users ADD COLUMN email_verified_at timestamptz;
