# API examples (generated from a real run)

Produced by running the actual server against the dev sample data and recording each response. Long lists are shortened with `... n more`; ids and timestamps differ per run.

Set up once:
```bash
BASE=http://localhost:8088/api        # through nginx (or http://localhost:8080 direct)
TOKEN=$(curl -s -X POST $BASE/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"..."}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')
```

The full contract (every parameter, status code and schema) is in [`openapi.yaml`](openapi.yaml).

## 1. Genres

Cached in Redis and by clients; ETag supports conditional requests.

```bash
curl -s "$BASE/v1/genres"
```

```
HTTP 200 OK
Cache-Control: public, max-age=300, stale-while-revalidate=600
Etag: "e3787b5f765dbfc95e171a19054232fb"
{
  "items": [
    {
      "slug": "action",
      "name": "Action",
      "title_count": 0
    },
    {
      "slug": "comedy",
      "name": "Comedy",
      "title_count": 2
    },
    "... 8 more"
  ]
}
```

## 2. List titles, page 1

`next_cursor` is opaque; send it back as `cursor` for the next page. It is `null` on the last page.

```bash
curl -s "$BASE/v1/titles?limit=2"
```

```
HTTP 200 OK
Cache-Control: public, max-age=30, stale-while-revalidate=60
Etag: "7886a666b2b7a9a39af626d1c33b22ab"
{
  "items": [
    {
      "id": "378f98da-e00c-4fa2-9233-b20d66e3c263",
      "slug": "sample-naija-hustle",
      "type": "series",
      "name": "Naija Hustle",
      "release_year": 2024,
      "maturity_rating": "12A",
      "runtime_minutes": null,
      "original_language": "pcm",
      "poster_url": "https://seed.invalid/posters/sample-naija-hustle.jpg",
      "status": "published",
      "published_at": "2026-10-07T06:28:56.451117Z",
      "genres": [
        "comedy"
      ]
    },
    {
      "id": "28a9f0cf-cbda-4494-856a-bf031b374188",
      "slug": "sample-lagos-after-midnight",
      "type": "movie",
      "name": "Lagos After Midnight",
      "release_year": 2023,
      "maturity_rating": "15",
      "runtime_minutes": 112,
      "original_language": "en",
      "poster_url": "https://seed.invalid/posters/sample-lagos-after-midnight.jpg",
      "status": "published",
      "published_at": "2026-10-05T06:28:56.451117Z",
      "genres": [
        "drama",
        "thriller"
      ]
    }
  ],
  "next_cursor": "eyJrIjoidGl0bGVzIiwicyI6Im5ld2VzdCIsInQiOiIyMDI2LTEwLTA1VDA2..."
}
```

## 3. List titles, page 2 (cursor)

```bash
curl -s "$BASE/v1/titles?limit=2&cursor=$NEXT_CURSOR"
```

```
HTTP 200 OK
{
  "items": [
    {
      "id": "a19a05fb-22b1-419b-9560-02a299e14d37",
      "slug": "sample-wahala-dey-sell",
      "type": "movie",
      "name": "Wahala Dey Sell",
      "release_year": 2024,
      "maturity_rating": "12A",
      "runtime_minutes": 94,
      "original_language": "pcm",
      "poster_url": "https://seed.invalid/posters/sample-wahala-dey-sell.jpg",
      "status": "published",
      "published_at": "2026-09-28T06:28:56.451117Z",
      "genres": [
        "comedy"
      ]
    },
    {
      "id": "4e881cdf-bf27-4058-998e-8c991f3ed887",
      "slug": "sample-ilu-oke",
      "type": "series",
      "name": "Ìlú Òkè",
      "release_year": 2023,
      "maturity_rating": "15",
      "runtime_minutes": null,
      "original_language": "yo",
      "poster_url": "https://seed.invalid/posters/sample-ilu-oke.jpg",
      "status": "published",
      "published_at": "2026-09-20T06:28:56.451117Z",
      "genres": [
        "drama",
        "epic-historical"
      ]
    }
  ],
  "next_cursor": "eyJrIjoidGl0bGVzIiwicyI6Im5ld2VzdCIsInQiOiIyMDI2LTA5LTIwVDA2..."
}
```

## 4. Filters: type, language, genre

```bash
curl -s "$BASE/v1/titles?type=series&language=yo&genre=drama"
```

```
HTTP 200 OK
{
  "items": [
    {
      "id": "4e881cdf-bf27-4058-998e-8c991f3ed887",
      "slug": "sample-ilu-oke",
      "type": "series",
      "name": "Ìlú Òkè",
      "release_year": 2023,
      "maturity_rating": "15",
      "runtime_minutes": null,
      "original_language": "yo",
      "poster_url": "https://seed.invalid/posters/sample-ilu-oke.jpg",
      "status": "published",
      "published_at": "2026-09-20T06:28:56.451117Z",
      "genres": [
        "drama",
        "epic-historical"
      ]
    }
  ],
  "next_cursor": null
}
```

## 5. Search (accent-insensitive: finds "Ọmọ Aládé")

```bash
curl -s "$BASE/v1/titles?q=omo%20alade"
```

```
HTTP 200 OK
{
  "items": [
    {
      "id": "766943fe-9ccd-4f27-a7d3-b5d52b065f45",
      "slug": "sample-omo-alade",
      "type": "movie",
      "name": "Ọmọ Aládé",
      "release_year": 2021,
      "maturity_rating": "PG",
      "runtime_minutes": 128,
      "original_language": "yo",
      "poster_url": "https://seed.invalid/posters/sample-omo-alade.jpg",
      "status": "published",
      "published_at": "2026-08-31T06:28:56.451117Z",
      "genres": [
        "drama",
        "epic-historical"
      ]
    }
  ],
  "next_cursor": null
}
```

## 6. Validation error

All field problems are reported together.

```bash
curl -s "$BASE/v1/titles?limit=0"
```

```
HTTP 422 Unprocessable Entity
{
  "error": {
    "code": "validation_failed",
    "message": "one or more fields are invalid",
    "request_id": "dc59bf360a1f0cb5",
    "fields": {
      "limit": "must be a whole number from 1 to 50"
    }
  }
}
```

## 7. Bad cursor

```bash
curl -s "$BASE/v1/titles?cursor=garbage"
```

```
HTTP 400 Bad Request
{
  "error": {
    "code": "invalid_cursor",
    "message": "the cursor is not valid for this request",
    "request_id": "e517336023280c72"
  }
}
```

## 8. Title detail: movie

`playable` is true once an HLS asset is ready; `subtitles` lists the tracks.

```bash
curl -s "$BASE/v1/titles/sample-omo-alade"
```

```
HTTP 200 OK
{
  "id": "766943fe-9ccd-4f27-a7d3-b5d52b065f45",
  "slug": "sample-omo-alade",
  "type": "movie",
  "name": "Ọmọ Aládé",
  "release_year": 2021,
  "maturity_rating": "PG",
  "runtime_minutes": 128,
  "original_language": "yo",
  "poster_url": "https://seed.invalid/posters/sample-omo-alade.jpg",
  "status": "published",
  "published_at": "2026-08-31T06:28:56.451117Z",
  "synopsis": "[SAMPLE DATA] A young prince returns to a kingdom that has f...",
  "backdrop_url": "https://seed.invalid/backdrops/sample-omo-alade.jpg",
  "genres": [
    {
      "slug": "drama",
      "name": "Drama"
    },
    {
      "slug": "epic-historical",
      "name": "Epic & Historical"
    }
  ],
  "directors": [
    {
      "person_id": "a4a14fb5-894f-4cb0-807a-0715cd2fc25f",
      "name": "Folake Adeyemi",
      "photo_url": "https://seed.invalid/people/folake-adeyemi.jpg",
      "character": null,
      "order": 0
    }
  ],
  "writers": [],
  "cast": [
    {
      "person_id": "87b24878-8641-46f2-b661-e383e605b50d",
      "name": "Tunde Balogun",
      "photo_url": "https://seed.invalid/people/tunde-balogun.jpg",
      "character": "Prince Aládé",
      "order": 0
    }
  ],
  "playable": false,
  "subtitles": [],
  "seasons": []
}
```

## 9. Title detail: series (seasons and episodes)

```bash
curl -s "$BASE/v1/titles/sample-ilu-oke"
```

```
HTTP 200 OK
{
  "id": "4e881cdf-bf27-4058-998e-8c991f3ed887",
  "slug": "sample-ilu-oke",
  "type": "series",
  "name": "Ìlú Òkè",
  "release_year": 2023,
  "maturity_rating": "15",
  "runtime_minutes": null,
  "original_language": "yo",
  "poster_url": "https://seed.invalid/posters/sample-ilu-oke.jpg",
  "status": "published",
  "published_at": "2026-09-20T06:28:56.451117Z",
  "synopsis": "[SAMPLE DATA] Rival families on a Yoruba hilltop town fight ...",
  "backdrop_url": "https://seed.invalid/backdrops/sample-ilu-oke.jpg",
  "genres": [
    {
      "slug": "drama",
      "name": "Drama"
    },
    {
      "slug": "epic-historical",
      "name": "Epic & Historical"
    }
  ],
  "directors": [],
  "writers": [],
  "cast": [
    {
      "person_id": "a4a14fb5-894f-4cb0-807a-0715cd2fc25f",
      "name": "Folake Adeyemi",
      "photo_url": "https://seed.invalid/people/folake-adeyemi.jpg",
      "character": "Iyalode",
      "order": 0
    },
    {
      "person_id": "87b24878-8641-46f2-b661-e383e605b50d",
      "name": "Tunde Balogun",
      "photo_url": "https://seed.invalid/people/tunde-balogun.jpg",
      "character": "Baálẹ̀",
      "order": 1
    }
  ],
  "playable": false,
  "subtitles": [],
  "seasons": [
    {
      "number": 1,
      "name": "Season 1",
      "synopsis": null,
      "episodes": [
        {
          "id": "0c22634e-5447-46f4-bbac-466c295157e0",
          "number": 1,
          "name": "Episode 1",
          "synopsis": null,
          "runtime_minutes": 35,
          "air_date": null,
          "playable": false,
          "subtitles": []
        },
        {
          "id": "ab6a8941-aa3d-4e4b-a906-c81405c546e5",
          "number": 2,
          "name": "Episode 2",
          "synopsis": null,
          "runtime_minutes": 35,
          "air_date": null,
          "playable": false,
          "subtitles": []
        },
        "... 1 more"
      ]
    },
    {
      "number": 2,
      "name": "Season 2",
      "synopsis": null,
      "episodes": [
        {
          "id": "3e57dfbb-0164-467c-bbca-602ec144d604",
          "number": 1,
          "name": "Episode 1",
          "synopsis": null,
          "runtime_minutes": 35,
          "air_date": null,
          "playable": false,
          "subtitles": []
        },
        {
          "id": "5a04aafd-24cb-4c60-b97a-17ce38c14531",
          "number": 2,
          "name": "Episode 2",
          "synopsis": null,
          "runtime_minutes": 35,
          "air_date": null,
          "playable": false,
          "subtitles": []
        },
        "... 1 more"
      ]
    }
  ]
}
```

## 10. A draft is invisible to the public

A draft looks exactly like a title that does not exist.

```bash
curl -s "$BASE/v1/titles/sample-kasuwa"
```

```
HTTP 404 Not Found
{
  "error": {
    "code": "title_not_found",
    "message": "title not found",
    "request_id": "6f5bdba896caac86"
  }
}
```

## 11. Home (anonymous)

Rows: Trending Now, New Releases, one per language. Cached in Redis (5 min) and shared.

```bash
curl -s "$BASE/v1/home"
```

```
HTTP 200 OK
Cache-Control: public, max-age=60, stale-while-revalidate=120
Etag: "4434a8987e542f0f3611e1470afdcbd3"
{
  "rows": [
    {
      "key": "trending",
      "kind": "titles",
      "title": "Trending Now",
      "items": [
        {
          "id": "378f98da-e00c-4fa2-9233-b20d66e3c263",
          "slug": "sample-naija-hustle",
          "type": "series",
          "name": "Naija Hustle",
          "release_year": 2024,
          "maturity_rating": "12A",
          "runtime_minutes": null,
          "original_language": "pcm",
          "poster_url": "https://seed.invalid/posters/sample-naija-hustle.jpg",
          "status": "published",
          "published_at": "2026-10-07T06:28:56.451117Z",
          "genres": [
            "comedy"
          ]
        },
        {
          "id": "28a9f0cf-cbda-4494-856a-bf031b374188",
          "slug": "sample-lagos-after-midnight",
          "type": "movie",
          "name": "Lagos After Midnight",
          "release_year": 2023,
          "maturity_rating": "15",
          "runtime_minutes": 112,
          "original_language": "en",
          "poster_url": "https://seed.invalid/posters/sample-lagos-after-midnight.jpg",
          "status": "published",
          "published_at": "2026-10-05T06:28:56.451117Z",
          "genres": [
            "drama",
            "thriller"
          ]
        },
        "... 7 more"
      ]
    },
    {
      "key": "new-releases",
      "kind": "titles",
      "title": "New Releases",
      "items": [
        {
          "id": "378f98da-e00c-4fa2-9233-b20d66e3c263",
          "slug": "sample-naija-hustle",
          "type": "series",
          "name": "Naija Hustle",
          "release_year": 2024,
          "maturity_rating": "12A",
          "runtime_minutes": null,
          "original_language": "pcm",
          "poster_url": "https://seed.invalid/posters/sample-naija-hustle.jpg",
          "status": "published",
          "published_at": "2026-10-07T06:28:56.451117Z",
          "genres": [
            "comedy"
          ]
        },
        {
          "id": "28a9f0cf-cbda-4494-856a-bf031b374188",
          "slug": "sample-lagos-after-midnight",
          "type": "movie",
          "name": "Lagos After Midnight",
          "release_year": 2023,
          "maturity_rating": "15",
          "runtime_minutes": 112,
          "original_language": "en",
          "poster_url": "https://seed.invalid/posters/sample-lagos-after-midnight.jpg",
          "status": "published",
          "published_at": "2026-10-05T06:28:56.451117Z",
          "genres": [
            "drama",
            "thriller"
          ]
        },
        "... 7 more"
      ]
    },
    "... 5 more"
  ]
}
```

## 12. Conditional GET (ETag)

The client already has this version: 304 with no body.

```bash
curl -s "$BASE/v1/genres" -H 'If-None-Match: "e3787b5f765dbfc95e171a19054232fb"'
```

```
HTTP 304 Not Modified
Cache-Control: public, max-age=300, stale-while-revalidate=600
Etag: "e3787b5f765dbfc95e171a19054232fb"
```

## 13. Watchlist (empty)

```bash
curl -s "$BASE/v1/me/watchlist" -H "Authorization: Bearer $TOKEN"
```

```
HTTP 200 OK
Cache-Control: private, no-store
{
  "items": [],
  "next_cursor": null
}
```

## 14. Add to watchlist

201 when added, 200 when it was already there (idempotent).

```bash
curl -s -X POST "$BASE/v1/me/watchlist" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"title_id": "378f98da-e00c-4fa2-9233-b20d66e3c263"}'
```

```
HTTP 201 Created
{
  "title_id": "378f98da-e00c-4fa2-9233-b20d66e3c263",
  "added_at": "2026-10-10T06:28:59.654361Z"
}
```

## 15. Add again (idempotent)

```bash
curl -s -X POST "$BASE/v1/me/watchlist" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"title_id": "378f98da-e00c-4fa2-9233-b20d66e3c263"}'
```

```
HTTP 200 OK
{
  "title_id": "378f98da-e00c-4fa2-9233-b20d66e3c263",
  "added_at": "2026-10-10T06:28:59.654361Z"
}
```

## 16. Watchlist

```bash
curl -s "$BASE/v1/me/watchlist" -H "Authorization: Bearer $TOKEN"
```

```
HTTP 200 OK
{
  "items": [
    {
      "id": "378f98da-e00c-4fa2-9233-b20d66e3c263",
      "slug": "sample-naija-hustle",
      "type": "series",
      "name": "Naija Hustle",
      "release_year": 2024,
      "maturity_rating": "12A",
      "runtime_minutes": null,
      "original_language": "pcm",
      "poster_url": "https://seed.invalid/posters/sample-naija-hustle.jpg",
      "status": "published",
      "published_at": "2026-10-07T06:28:56.451117Z",
      "genres": [
        "comedy"
      ],
      "added_at": "2026-10-10T06:28:59.654361Z"
    }
  ],
  "next_cursor": null
}
```

## 17. Remove from watchlist

```bash
curl -s -X DELETE "$BASE/v1/me/watchlist/$TITLE_ID" -H "Authorization: Bearer $TOKEN"
```

```
HTTP 204 No Content
```

## 18. None

```bash
curl -s "$BASE/v1/titles/sample-omo-alade"
```

```
HTTP 200 OK
{
  "id": "766943fe-9ccd-4f27-a7d3-b5d52b065f45",
  "slug": "sample-omo-alade",
  "type": "movie",
  "name": "Ọmọ Aládé",
  "release_year": 2021,
  "maturity_rating": "PG",
  "runtime_minutes": 128,
  "original_language": "yo",
  "poster_url": "https://seed.invalid/posters/sample-omo-alade.jpg",
  "status": "published",
  "published_at": "2026-08-31T06:28:56.451117Z",
  "synopsis": "[SAMPLE DATA] A young prince returns to a kingdom that has f...",
  "backdrop_url": "https://seed.invalid/backdrops/sample-omo-alade.jpg",
  "genres": [
    {
      "slug": "drama",
      "name": "Drama"
    },
    {
      "slug": "epic-historical",
      "name": "Epic & Historical"
    }
  ],
  "directors": [
    {
      "person_id": "a4a14fb5-894f-4cb0-807a-0715cd2fc25f",
      "name": "Folake Adeyemi",
      "photo_url": "https://seed.invalid/people/folake-adeyemi.jpg",
      "character": null,
      "order": 0
    }
  ],
  "writers": [],
  "cast": [
    {
      "person_id": "87b24878-8641-46f2-b661-e383e605b50d",
      "name": "Tunde Balogun",
      "photo_url": "https://seed.invalid/people/tunde-balogun.jpg",
      "character": "Prince Aládé",
      "order": 0
    }
  ],
  "playable": false,
  "subtitles": [],
  "seasons": []
}
```

## 19. None

```bash
curl -s "$BASE/v1/titles/sample-ilu-oke"
```

```
HTTP 200 OK
{
  "id": "4e881cdf-bf27-4058-998e-8c991f3ed887",
  "slug": "sample-ilu-oke",
  "type": "series",
  "name": "Ìlú Òkè",
  "release_year": 2023,
  "maturity_rating": "15",
  "runtime_minutes": null,
  "original_language": "yo",
  "poster_url": "https://seed.invalid/posters/sample-ilu-oke.jpg",
  "status": "published",
  "published_at": "2026-09-20T06:28:56.451117Z",
  "synopsis": "[SAMPLE DATA] Rival families on a Yoruba hilltop town fight ...",
  "backdrop_url": "https://seed.invalid/backdrops/sample-ilu-oke.jpg",
  "genres": [
    {
      "slug": "drama",
      "name": "Drama"
    },
    {
      "slug": "epic-historical",
      "name": "Epic & Historical"
    }
  ],
  "directors": [],
  "writers": [],
  "cast": [
    {
      "person_id": "a4a14fb5-894f-4cb0-807a-0715cd2fc25f",
      "name": "Folake Adeyemi",
      "photo_url": "https://seed.invalid/people/folake-adeyemi.jpg",
      "character": "Iyalode",
      "order": 0
    },
    {
      "person_id": "87b24878-8641-46f2-b661-e383e605b50d",
      "name": "Tunde Balogun",
      "photo_url": "https://seed.invalid/people/tunde-balogun.jpg",
      "character": "Baálẹ̀",
      "order": 1
    }
  ],
  "playable": false,
  "subtitles": [],
  "seasons": [
    {
      "number": 1,
      "name": "Season 1",
      "synopsis": null,
      "episodes": [
        {
          "id": "0c22634e-5447-46f4-bbac-466c295157e0",
          "number": 1,
          "name": "Episode 1",
          "synopsis": null,
          "runtime_minutes": 35,
          "air_date": null,
          "playable": false,
          "subtitles": []
        },
        {
          "id": "ab6a8941-aa3d-4e4b-a906-c81405c546e5",
          "number": 2,
          "name": "Episode 2",
          "synopsis": null,
          "runtime_minutes": 35,
          "air_date": null,
          "playable": false,
          "subtitles": []
        },
        "... 1 more"
      ]
    },
    {
      "number": 2,
      "name": "Season 2",
      "synopsis": null,
      "episodes": [
        {
          "id": "3e57dfbb-0164-467c-bbca-602ec144d604",
          "number": 1,
          "name": "Episode 1",
          "synopsis": null,
          "runtime_minutes": 35,
          "air_date": null,
          "playable": false,
          "subtitles": []
        },
        {
          "id": "5a04aafd-24cb-4c60-b97a-17ce38c14531",
          "number": 2,
          "name": "Episode 2",
          "synopsis": null,
          "runtime_minutes": 35,
          "air_date": null,
          "playable": false,
          "subtitles": []
        },
        "... 1 more"
      ]
    }
  ]
}
```

## 20. Save progress (movie)

Throttled to 30/min per user. Idempotent: the same values change nothing. Sub-second decimals are floored.

```bash
curl -s -X PUT "$BASE/v1/me/progress" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"title_id": "766943fe-9ccd-4f27-a7d3-b5d52b065f45", "position_seconds": 1234.5, "duration_seconds": 7680}'
```

```
HTTP 200 OK
Cache-Control: private, no-store
{
  "title_id": "766943fe-9ccd-4f27-a7d3-b5d52b065f45",
  "episode_id": null,
  "position_seconds": 1234,
  "duration_seconds": 7680,
  "completed": false,
  "updated_at": "2026-10-10T06:28:59.688913Z"
}
```

## 21. Save progress (series episode)

```bash
curl -s -X PUT "$BASE/v1/me/progress" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"title_id": "4e881cdf-bf27-4058-998e-8c991f3ed887", "episode_id": "0c22634e-5447-46f4-bbac-466c295157e0", "position_seconds": 600, "duration_seconds": 2100}'
```

```
HTTP 200 OK
{
  "title_id": "4e881cdf-bf27-4058-998e-8c991f3ed887",
  "episode_id": "0c22634e-5447-46f4-bbac-466c295157e0",
  "position_seconds": 600,
  "duration_seconds": 2100,
  "completed": false,
  "updated_at": "2026-10-10T06:28:59.6959Z"
}
```

## 22. Reaching 95% marks it completed

`completed` is decided by the server; clients cannot set it.

```bash
curl -s -X PUT "$BASE/v1/me/progress" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"title_id": "766943fe-9ccd-4f27-a7d3-b5d52b065f45", "position_seconds": 7400, "duration_seconds": 7680}'
```

```
HTTP 200 OK
{
  "title_id": "766943fe-9ccd-4f27-a7d3-b5d52b065f45",
  "episode_id": null,
  "position_seconds": 7400,
  "duration_seconds": 7680,
  "completed": true,
  "updated_at": "2026-10-10T06:28:59.701092Z"
}
```

## 23. Series without an episode_id

```bash
curl -s -X PUT "$BASE/v1/me/progress" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"title_id": "4e881cdf-bf27-4058-998e-8c991f3ed887", "position_seconds": 1, "duration_seconds": 10}'
```

```
HTTP 422 Unprocessable Entity
{
  "error": {
    "code": "validation_failed",
    "message": "one or more fields are invalid",
    "request_id": "79b41350c73dd525",
    "fields": {
      "episode_id": "required for a series, not allowed for a movie, and must bel..."
    }
  }
}
```

## 24. Continue watching

Finished titles are excluded; a series appears once, at the episode watched last.

```bash
curl -s "$BASE/v1/me/continue-watching" -H "Authorization: Bearer $TOKEN"
```

```
HTTP 200 OK
{
  "items": [
    {
      "title": {
        "id": "4e881cdf-bf27-4058-998e-8c991f3ed887",
        "slug": "sample-ilu-oke",
        "type": "series",
        "name": "Ìlú Òkè",
        "release_year": 2023,
        "maturity_rating": "15",
        "runtime_minutes": null,
        "original_language": "yo",
        "poster_url": "https://seed.invalid/posters/sample-ilu-oke.jpg",
        "status": "published",
        "published_at": "2026-09-20T06:28:56.451117Z",
        "genres": [
          "drama",
          "epic-historical"
        ]
      },
      "episode": {
        "id": "0c22634e-5447-46f4-bbac-466c295157e0",
        "season_number": 1,
        "number": 1,
        "name": "Episode 1"
      },
      "position_seconds": 600,
      "duration_seconds": 2100,
      "progress_percent": 28,
      "updated_at": "2026-10-10T06:28:59.6959Z"
    }
  ]
}
```

## 25. Home (signed in): Continue Watching comes first

```bash
curl -s "$BASE/v1/home" -H "Authorization: Bearer $TOKEN"
```

```
HTTP 200 OK
Cache-Control: private, no-cache
{
  "rows": [
    {
      "key": "continue-watching",
      "kind": "continue",
      "title": "Continue Watching",
      "continue": [
        {
          "title": {
            "id": "4e881cdf-bf27-4058-998e-8c991f3ed887",
            "slug": "sample-ilu-oke",
            "type": "series",
            "name": "Ìlú Òkè",
            "release_year": 2023,
            "maturity_rating": "15",
            "runtime_minutes": null,
            "original_language": "yo",
            "poster_url": "https://seed.invalid/posters/sample-ilu-oke.jpg",
            "status": "published",
            "published_at": "2026-09-20T06:28:56.451117Z",
            "genres": [
              "drama",
              "epic-historical"
            ]
          },
          "episode": {
            "id": "0c22634e-5447-46f4-bbac-466c295157e0",
            "season_number": 1,
            "number": 1,
            "name": "Episode 1"
          },
          "position_seconds": 600,
          "duration_seconds": 2100,
          "progress_percent": 28,
          "updated_at": "2026-10-10T06:28:59.6959Z"
        }
      ]
    },
    {
      "key": "trending",
      "kind": "titles",
      "title": "Trending Now",
      "items": [
        {
          "id": "378f98da-e00c-4fa2-9233-b20d66e3c263",
          "slug": "sample-naija-hustle",
          "type": "series",
          "name": "Naija Hustle",
          "release_year": 2024,
          "maturity_rating": "12A",
          "runtime_minutes": null,
          "original_language": "pcm",
          "poster_url": "https://seed.invalid/posters/sample-naija-hustle.jpg",
          "status": "published",
          "published_at": "2026-10-07T06:28:56.451117Z",
          "genres": [
            "comedy"
          ]
        },
        {
          "id": "28a9f0cf-cbda-4494-856a-bf031b374188",
          "slug": "sample-lagos-after-midnight",
          "type": "movie",
          "name": "Lagos After Midnight",
          "release_year": 2023,
          "maturity_rating": "15",
          "runtime_minutes": 112,
          "original_language": "en",
          "poster_url": "https://seed.invalid/posters/sample-lagos-after-midnight.jpg",
          "status": "published",
          "published_at": "2026-10-05T06:28:56.451117Z",
          "genres": [
            "drama",
            "thriller"
          ]
        },
        "... 7 more"
      ]
    },
    "... 6 more"
  ]
}
```

## 26. A bad token on a public endpoint is a 401

So an app holding a stale token learns to refresh instead of being silently treated as anonymous.

```bash
curl -s "$BASE/v1/titles" -H "Authorization: Bearer $TOKEN"
```

```
HTTP 401 Unauthorized
{
  "error": {
    "code": "invalid_token",
    "message": "invalid access token",
    "request_id": "a95886b352fbdc9b"
  }
}
```

## 27. Protected endpoint without a token

```bash
curl -s "$BASE/v1/me/watchlist"
```

```
HTTP 401 Unauthorized
{
  "error": {
    "code": "unauthenticated",
    "message": "authentication required",
    "request_id": "150d2e859ff74a63"
  }
}
```

## 28. An admin (or the owning creator) can see the draft

Roles ride in the access token, so a promotion applies after the next sign-in or refresh.

```bash
curl -s "$BASE/v1/titles/sample-kasuwa" -H "Authorization: Bearer $TOKEN"
```

```
HTTP 200 OK
{
  "id": "91a1e712-8873-4783-92a9-e6eb3b97eb97",
  "slug": "sample-kasuwa",
  "type": "series",
  "name": "Kasuwa",
  "release_year": 2023,
  "maturity_rating": "PG",
  "runtime_minutes": null,
  "original_language": "ha",
  "poster_url": "https://seed.invalid/posters/sample-kasuwa.jpg",
  "status": "draft",
  "published_at": null,
  "synopsis": "[SAMPLE DATA] Traders in a busy Hausa market compete, cooper...",
  "backdrop_url": "https://seed.invalid/backdrops/sample-kasuwa.jpg",
  "genres": [
    {
      "slug": "comedy",
      "name": "Comedy"
    }
  ],
  "directors": [],
  "writers": [],
  "cast": [],
  "playable": false,
  "subtitles": [],
  "seasons": [
    {
      "number": 1,
      "name": "Season 1",
      "synopsis": null,
      "episodes": [
        {
          "id": "db06e39b-a5ab-4009-a425-3cd570eb7420",
          "number": 1,
          "name": "Episode 1",
          "synopsis": null,
          "runtime_minutes": 35,
          "air_date": null,
          "playable": false,
          "subtitles": []
        },
        {
          "id": "14deafd7-8461-469c-8836-ebf86858c4cb",
          "number": 2,
          "name": "Episode 2",
          "synopsis": null,
          "runtime_minutes": 35,
          "air_date": null,
          "playable": false,
          "subtitles": []
        },
        "... 1 more"
      ]
    }
  ],
  "viewer": {
    "in_watchlist": false,
    "progress": []
  }
}
```
