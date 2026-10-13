-- =====================================================================================
-- SAMPLE DATA: DEVELOPMENT ONLY. Bulk rows so EXPLAIN shows realistic plans
-- (with a dozen rows Postgres correctly prefers a sequential scan).
-- Run with:  make seed-bulk     Remove with: make seed-bulk-clean
-- =====================================================================================
\if :{?sample_ok}
\else
  \echo 'Refusing to run: dev-only. Use "make seed-bulk".'
  \quit
\endif
\set ON_ERROR_STOP on
BEGIN;
SELECT setseed(0.42);

DELETE FROM titles WHERE slug LIKE 'bulk-%';
DELETE FROM users  WHERE email LIKE 'bulk-%@seed.invalid';

INSERT INTO users (email, password_hash, role)
SELECT 'bulk-' || n || '@seed.invalid', '!bulk-no-login', CASE WHEN n = 1 THEN 'creator'::user_role ELSE 'viewer'::user_role END
FROM generate_series(1, 2000) n;
INSERT INTO profiles (user_id, display_name)
SELECT id, 'Bulk user' FROM users WHERE email LIKE 'bulk-%@seed.invalid';

INSERT INTO titles (type, slug, name, synopsis, release_year, maturity_rating, runtime_minutes, original_language,
                    status, creator_id, published_at)
SELECT 'movie', 'bulk-' || n,
       (ARRAY['Lagos','Accra','Kano','Freetown','Enugu','Ibadan','Kumasi','Abuja'])[1 + n % 8] || ' ' ||
       (ARRAY['Nights','Hustle','Dreams','Market','Story','Road','Wedding','Harvest'])[1 + (n / 8) % 8] || ' ' || n,
       '[SAMPLE DATA] Bulk performance row ' || n || ' about family, money and music.',
       2000 + n % 25, (ARRAY['G','PG','12','15'])[1 + n % 4], 80 + n % 60,
       (ARRAY['en','yo','ig','ha','pcm'])[1 + n % 5],
       CASE WHEN n % 10 < 8 THEN 'published'::title_status WHEN n % 10 = 8 THEN 'draft' ELSE 'unpublished' END,
       (SELECT id FROM users WHERE email = 'bulk-1@seed.invalid'),
       now() - make_interval(mins => (random() * 1051200)::int)
FROM generate_series(1, 20000) n;

INSERT INTO title_genres (title_id, genre_id)
SELECT DISTINCT t.id, 1 + ((substr(t.slug, 6))::int + k) % 10
FROM titles t CROSS JOIN generate_series(0, 2) k
WHERE t.slug LIKE 'bulk-%';

INSERT INTO watchlist (user_id, title_id, added_at)
SELECT u.id, t.id, now() - make_interval(mins => (random() * 100000)::int)
FROM users u
JOIN LATERAL (SELECT id FROM titles WHERE slug LIKE 'bulk-%' ORDER BY md5(id::text || u.id::text) LIMIT 20) t ON true
WHERE u.email LIKE 'bulk-%@seed.invalid';

INSERT INTO watch_progress (user_id, title_id, position_seconds, duration_seconds, completed, updated_at)
SELECT u.id, t.id, 600, 6000, (random() < 0.4), now() - make_interval(mins => (random() * 100000)::int)
FROM users u
JOIN LATERAL (SELECT id FROM titles WHERE slug LIKE 'bulk-%' ORDER BY md5(u.id::text || id::text) LIMIT 10) t ON true
WHERE u.email LIKE 'bulk-%@seed.invalid';

COMMIT;
ANALYZE;
\echo 'Bulk data loaded: 20000 titles, 2000 users, ~40000 watchlist rows, ~20000 progress rows.'
