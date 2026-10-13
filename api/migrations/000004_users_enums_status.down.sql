ALTER TABLE users DROP COLUMN email_verified_at;
ALTER TABLE users DROP COLUMN status;

ALTER TABLE users ALTER COLUMN role DROP DEFAULT;
ALTER TABLE users ALTER COLUMN role TYPE text USING role::text;
ALTER TABLE users ALTER COLUMN role SET DEFAULT 'viewer';
ALTER TABLE users ADD CONSTRAINT users_role_valid CHECK (role IN ('viewer', 'creator', 'admin'));

DROP TYPE user_status;
DROP TYPE user_role;
