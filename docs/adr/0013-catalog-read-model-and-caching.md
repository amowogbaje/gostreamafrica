# ADR-0013: Catalog read model: visibility, keyset pagination and an epoch-versioned cache
Status: Accepted

## Context
Viewers browse thousands of titles on mobile data. Responses must be small, pages must stay correct while the catalog changes, drafts must never leak, and the home page and genre list are hit constantly but change rarely. Watchlist and watch progress are read together with titles on every screen.

## Decision
- **Visibility is one SQL predicate applied everywhere** (list, search, detail, watchlist, continue watching, progress): anonymous visitors and viewers see `published`; a creator also sees titles they own in any state; an admin sees all. A hidden title answers 404, exactly like a missing one. A role without a verified user id is treated as anonymous.
- **Keyset (cursor) pagination**, never OFFSET. Orders are `(published_at DESC NULLS FIRST, id)`, `(name, id)` and `(relevance, id)`; the cursor holds the last row's keys. It is opaque, bound to its sort, and carries no authority (all values are typed parameters). Page size is 1 to 50 (default 20); out-of-range is a 422, not a silent clamp.
- **Separate DTOs:** list cards (`TitleSummary`: no synopsis or backdrop) and the full `TitleDetail`. Genres on cards are slugs.
- **Home** is the public rows (Trending Now by distinct viewers with progress activity in the last 7 days, padded with the newest titles; New Releases; one row per language, top 5 languages) plus a live, uncached Continue Watching row for signed-in users.
- **Redis cache for `/v1/home` (public part, 5 min) and `/v1/genres` (10 min)** with explicit invalidation by *epoch*: entries live under the current epoch and `TitlesChanged` bumps it, so everything becomes unreachable at once. A reader that loaded the old epoch writes under the old epoch, so a slow reader can never put stale data back. Concurrent cold misses are collapsed to one database build (singleflight). A cache outage never fails a request. Every write path that changes what viewers see must call `catalog.Service.TitlesChanged`.
- **HTTP caching:** ETag on every cacheable GET with `If-None-Match` support (304). Anonymous responses are `public` with a short `max-age` and `stale-while-revalidate`; anything signed-in is `private, no-cache`; per-user endpoints are `no-store`. `Vary: Authorization` throughout.
- **Progress:** `PUT /v1/me/progress` is idempotent (a repeat changes nothing, not even `updated_at`), clamps the position to the duration, marks `completed` at 95% (the server decides; a client cannot set it), and is throttled to 30 requests a minute per user.
- **Watchlist and progress live in the catalog module** for now because every read joins them to titles and applies the same visibility rule; they move to `playback` when it gains signed URLs.
- **The OpenAPI spec `docs/openapi.yaml` is the contract, enforced in CI** by `api/internal/contract`: the routes registered in code must equal the operations in the spec, and every operation is exercised through the production wiring with each request and response validated against its schema (undocumented fields fail).

## Consequences
- Pages stay correct under concurrent inserts and deletes, and cost the same on page 1 and page 500.
- Home and genres cost one Redis read on a hit; a title change shows up as soon as the write path calls `TitlesChanged`, or within the TTL otherwise.
- Writes to titles must remember to invalidate: the creators and admin modules will do so, and a missing call shows up as stale rows for at most five minutes.
- Adding a field or route means editing the spec in the same change, or CI fails.
- A very common genre filter walks the newest-first index; denormalize if genre browsing becomes hot at much larger scale.
- No `HEAD` support on these routes (GET only).
