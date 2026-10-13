-- Access-pattern queries. Run with:  make explain      (after: make seed-bulk)
-- Ids are passed as VALUES (psql \gset), the way the application passes query parameters.
-- With a subquery instead, the planner cannot see which genre/user it is and guesses an average.
-- Expect Index / Bitmap Index scans on the named indexes, never "Seq Scan on titles".
\pset pager off
SELECT id AS user_id      FROM users  WHERE email = 'bulk-7@seed.invalid' \gset
SELECT id AS common_genre FROM genres WHERE slug  = 'drama'               \gset

\echo '--- 1. Browse: published titles, newest first (page 1)        expect titles_status_published_idx'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF, SUMMARY OFF)
SELECT id, slug, name, published_at FROM titles
WHERE status = 'published' ORDER BY published_at DESC, id LIMIT 20;

\echo '--- 2a. By genre, COMMON genre (Drama): walks newest-first and probes membership   expect titles_status_published_idx + title_genres_pkey'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF, SUMMARY OFF)
SELECT t.id, t.slug, t.name, t.published_at
FROM title_genres tg JOIN titles t ON t.id = tg.title_id
WHERE tg.genre_id = :common_genre AND t.status = 'published'
ORDER BY t.published_at DESC LIMIT 20;

\echo '--- 2b. By genre, RARE genre: starts from the genre index          expect title_genres_genre_idx'
BEGIN;
INSERT INTO genres (slug, name) VALUES ('rare-test', 'Rare test (rolled back)');
INSERT INTO title_genres (title_id, genre_id)
  SELECT id, (SELECT id FROM genres WHERE slug = 'rare-test') FROM titles WHERE slug IN ('bulk-100', 'bulk-2000', 'bulk-9000', 'bulk-15000');
ANALYZE title_genres;
SELECT id AS rare_genre FROM genres WHERE slug = 'rare-test' \gset
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF, SUMMARY OFF)
SELECT t.id, t.slug, t.name, t.published_at
FROM title_genres tg JOIN titles t ON t.id = tg.title_id
WHERE tg.genre_id = :rare_genre AND t.status = 'published'
ORDER BY t.published_at DESC LIMIT 20;
ROLLBACK;

\echo '--- 3. Watchlist for one user, newest first                    expect watchlist_user_added_idx'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF, SUMMARY OFF)
SELECT w.title_id, w.added_at, t.name FROM watchlist w JOIN titles t ON t.id = w.title_id
WHERE w.user_id = :'user_id' ORDER BY w.added_at DESC LIMIT 20;

\echo '--- 4. Continue watching: unfinished items, most recent first   expect watch_progress_continue_idx'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF, SUMMARY OFF)
SELECT p.title_id, p.episode_id, p.position_seconds, p.updated_at
FROM watch_progress p
WHERE p.user_id = :'user_id' AND NOT p.completed
ORDER BY p.updated_at DESC LIMIT 10;

\echo '--- 5. Full-text search for one title (accent-insensitive, name weighted above synopsis)   expect titles_search_idx'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF, SUMMARY OFF)
SELECT id, slug, name, ts_rank(search_vector, q) AS rank
FROM titles, websearch_to_tsquery('simple', app_unaccent('accra story 4321')) q
WHERE status = 'published' AND search_vector @@ q
ORDER BY rank DESC LIMIT 20;

-- Note: a BROAD search such as 'lagos nights' matches about 1.5% of 20000 rows, and Postgres
-- correctly prefers a sequential scan there. The index is used once the match is selective or
-- the table grows. Prove it is usable with:
--   SET enable_seqscan = off; EXPLAIN SELECT 1 FROM titles WHERE search_vector @@ websearch_to_tsquery('simple','lagos nights');
