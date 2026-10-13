ALTER TABLE users ADD COLUMN name text NOT NULL DEFAULT 'User';
UPDATE users u SET name = p.display_name FROM profiles p WHERE p.user_id = u.id;
ALTER TABLE users ALTER COLUMN name DROP DEFAULT;
ALTER TABLE users ADD CONSTRAINT users_name_len CHECK (char_length(name) BETWEEN 1 AND 80);
DROP TABLE profiles;
