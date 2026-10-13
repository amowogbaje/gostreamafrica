-- =====================================================================================
-- SAMPLE DATA: DEVELOPMENT ONLY. Every title, person and synopsis below is FICTIONAL.
-- Markers: slugs start with "sample-", synopses start with "[SAMPLE DATA]", image URLs use
-- the reserved ".invalid" domain, and the owner account cannot log in.
-- Run with:  make seed        (it refuses unless APP_ENV=local)
-- Safe to re-run: it deletes its own sample rows first.
-- =====================================================================================
\if :{?sample_ok}
\else
  \echo 'Refusing to run: this is dev-only sample data. Use "make seed".'
  \quit
\endif
\set ON_ERROR_STOP on
BEGIN;

DELETE FROM titles WHERE slug LIKE 'sample-%';
DELETE FROM people WHERE photo_url LIKE 'https://seed.invalid/%';
DELETE FROM users  WHERE email = 'sample.creator@seed.invalid';

-- Owner of all sample titles. '!' is not a valid password hash, so nobody can log in as it.
WITH u AS (
    INSERT INTO users (email, password_hash, role, email_verified_at)
    VALUES ('sample.creator@seed.invalid', '!sample-account-no-login', 'creator', now())
    RETURNING id
)
INSERT INTO profiles (user_id, display_name, country, preferred_language)
SELECT id, 'Sample Creator (dev only)', 'NG', 'en' FROM u;

INSERT INTO titles (type, slug, name, synopsis, release_year, maturity_rating, runtime_minutes,
                    original_language, poster_url, backdrop_url, status, creator_id, published_at)
SELECT v.type::title_type, v.slug, v.name, '[SAMPLE DATA] ' || v.synopsis, v.year, v.rating, v.runtime,
       v.lang, 'https://seed.invalid/posters/' || v.slug || '.jpg', 'https://seed.invalid/backdrops/' || v.slug || '.jpg',
       v.status::title_status, (SELECT id FROM users WHERE email = 'sample.creator@seed.invalid'),
       CASE WHEN v.days_ago IS NULL THEN NULL ELSE now() - make_interval(days => v.days_ago) END
FROM (VALUES
 -- type,    slug,                       name,                      synopsis,                                                                                year, rating, runtime, lang, status,        days_ago
 ('movie',  'sample-lagos-after-midnight','Lagos After Midnight',   'A night-shift driver in Lagos picks up a passenger who knows too much.',                2023, '15',   112, 'en',  'published',   5),
 ('movie',  'sample-omo-alade',          'Ọmọ Aládé',               'A young prince returns to a kingdom that has forgotten his name.',                       2021, 'PG',   128, 'yo',  'published',   40),
 ('movie',  'sample-nne-oma',            'Nne Ọma',                 'Three generations of women in Enugu argue, forgive and cook for a wedding.',             2022, 'PG',    98, 'ig',  'published',   75),
 ('movie',  'sample-gidan-mafarki',      'Gidan Mafarki',           'Two families in Kano are joined by a marriage neither side planned.',                    2020, '12',   105, 'ha',  'published',   120),
 ('movie',  'sample-wahala-dey-sell',    'Wahala Dey Sell',         'A market trader in Onitsha turns a small lie into a very big business.',                 2024, '12A',   94, 'pcm', 'published',   12),
 ('movie',  'sample-kumasi-nights',      'Kumasi Nights',           'A musician in Kumasi has one weekend to win back the band he left.',                     2019, '12',   101, 'en',  'published',   200),
 ('movie',  'sample-salone-rising',      'Salone Rising',           'Young farmers and fishers rebuild their livelihoods along the Sierra Leone coast.',      2022, 'PG',    76, 'kri', 'unpublished', 300),
 ('series', 'sample-ilu-oke',            'Ìlú Òkè',                 'Rival families on a Yoruba hilltop town fight over land, love and a chieftaincy.',       2023, '15',  NULL, 'yo',  'published',   20),
 ('series', 'sample-naija-hustle',       'Naija Hustle',            'Four flatmates in Yaba chase side hustles and keep missing the rent.',                   2024, '12A', NULL, 'pcm', 'published',   3),
 ('series', 'sample-harmattan-heights',  'Harmattan Heights',       'A Kaduna family business is tested when the founder steps down.',                        2022, '12',  NULL, 'en',  'published',   150),
 ('series', 'sample-accra-calling',      'Accra Calling',           'Call-centre colleagues in Accra find friendship and trouble on the night shift.',        2021, '12',  NULL, 'en',  'processing',  NULL),
 ('series', 'sample-kasuwa',             'Kasuwa',                  'Traders in a busy Hausa market compete, cooperate and gossip.',                          2023, 'PG',  NULL, 'ha',  'draft',       NULL)
) AS v(type, slug, name, synopsis, year, rating, runtime, lang, status, days_ago);

INSERT INTO title_genres (title_id, genre_id)
SELECT t.id, g.id
FROM (VALUES
 ('sample-lagos-after-midnight','thriller'), ('sample-lagos-after-midnight','drama'),
 ('sample-omo-alade','epic-historical'),     ('sample-omo-alade','drama'),
 ('sample-nne-oma','family'),                ('sample-nne-oma','drama'),
 ('sample-gidan-mafarki','romance'),         ('sample-gidan-mafarki','drama'),
 ('sample-wahala-dey-sell','comedy'),
 ('sample-kumasi-nights','musical'),         ('sample-kumasi-nights','romance'),
 ('sample-salone-rising','documentary'),
 ('sample-ilu-oke','drama'),                 ('sample-ilu-oke','epic-historical'),
 ('sample-naija-hustle','comedy'),
 ('sample-harmattan-heights','drama'),       ('sample-harmattan-heights','family'),
 ('sample-accra-calling','comedy'),          ('sample-accra-calling','drama'),
 ('sample-kasuwa','comedy')
) AS m(slug, genre)
JOIN titles t ON t.slug = m.slug
JOIN genres g ON g.slug = m.genre;

INSERT INTO seasons (title_id, number, name)
SELECT t.id, s.n, 'Season ' || s.n
FROM (VALUES ('sample-ilu-oke', 2), ('sample-naija-hustle', 2), ('sample-harmattan-heights', 1),
             ('sample-accra-calling', 1), ('sample-kasuwa', 1)) AS v(slug, seasons)
JOIN titles t ON t.slug = v.slug
CROSS JOIN LATERAL generate_series(1, v.seasons) AS s(n);

INSERT INTO episodes (season_id, title_id, number, name, runtime_minutes)
SELECT se.id, se.title_id, e.n, 'Episode ' || e.n, 35
FROM seasons se
JOIN titles t ON t.id = se.title_id AND t.slug LIKE 'sample-%'
CROSS JOIN generate_series(1, 3) AS e(n);

INSERT INTO people (name, photo_url)
SELECT name, 'https://seed.invalid/people/' || lower(replace(name, ' ', '-')) || '.jpg'
FROM (VALUES ('Adaeze Nwosu'), ('Tunde Balogun'), ('Amina Bello'), ('Kofi Mensah'),
             ('Fatmata Kamara'), ('Chidi Eze'), ('Folake Adeyemi'), ('Yusuf Danjuma')) AS p(name);

INSERT INTO title_people (title_id, person_id, role, character_name, billing_order)
SELECT t.id, p.id, m.role::credit_role, m.character_name, m.ord
FROM (VALUES
 ('sample-lagos-after-midnight','Tunde Balogun','director',NULL,0),
 ('sample-lagos-after-midnight','Folake Adeyemi','cast','The Passenger',0),
 ('sample-lagos-after-midnight','Chidi Eze','cast','Femi, the driver',1),
 ('sample-omo-alade','Folake Adeyemi','director',NULL,0),
 ('sample-omo-alade','Tunde Balogun','cast','Prince Aládé',0),
 ('sample-nne-oma','Adaeze Nwosu','director',NULL,0),
 ('sample-nne-oma','Chidi Eze','writer',NULL,0),
 ('sample-gidan-mafarki','Yusuf Danjuma','director',NULL,0),
 ('sample-gidan-mafarki','Amina Bello','cast','Zainab',0),
 ('sample-kumasi-nights','Kofi Mensah','director',NULL,0),
 ('sample-kumasi-nights','Kofi Mensah','cast','Kwame',0),
 ('sample-salone-rising','Fatmata Kamara','director',NULL,0),
 ('sample-ilu-oke','Folake Adeyemi','cast','Iyalode',0),
 ('sample-ilu-oke','Tunde Balogun','cast','Baálẹ̀',1)
) AS m(slug, person, role, character_name, ord)
JOIN titles t ON t.slug = m.slug
JOIN people p ON p.name = m.person AND p.photo_url LIKE 'https://seed.invalid/%';

COMMIT;
\echo 'Sample data loaded: 12 titles (7 movies, 5 series).'
