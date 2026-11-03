-- Dev only: makes an existing account a creator and gives it one empty draft movie to upload a
-- video to. Usage: make dev-creator EMAIL=you@example.com
-- The account must already exist (register through the API or Swagger first). Safe to repeat:
-- a new draft movie is added only when the creator has no title without a video.
\set ON_ERROR_STOP on
SELECT set_config('app.email', :'email', false);

UPDATE users
   SET role = 'creator', email_verified_at = COALESCE(email_verified_at, now())
 WHERE email = current_setting('app.email')::citext;

DO $$
DECLARE
    uid uuid;
    tid uuid;
BEGIN
    SELECT id INTO uid FROM users WHERE email = current_setting('app.email')::citext;
    IF uid IS NULL THEN
        RAISE EXCEPTION 'no account with email %: register it first (POST /v1/auth/register)', current_setting('app.email');
    END IF;
    SELECT t.id INTO tid FROM titles t
     WHERE t.creator_id = uid AND t.type = 'movie' AND t.slug LIKE 'upload-test-%'
       AND NOT EXISTS (SELECT 1 FROM media_assets m WHERE m.title_id = t.id)
       AND NOT EXISTS (SELECT 1 FROM uploads u WHERE u.title_id = t.id AND u.status IN ('initiated', 'uploaded', 'processing'))
     LIMIT 1;
    IF tid IS NULL THEN
        INSERT INTO titles (type, slug, name, synopsis, original_language, status, creator_id)
        VALUES ('movie', 'upload-test-' || substr(md5(random()::text), 1, 8), 'Upload test movie',
                'An empty draft created by make dev-creator to try the upload flow.', 'en', 'draft', uid)
        RETURNING id INTO tid;
    END IF;
    RAISE NOTICE 'creator ready. Upload to title_id: %', tid;
END $$;
