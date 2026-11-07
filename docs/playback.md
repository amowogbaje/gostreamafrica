# Playback and signed media URLs (Phase 2.3)

Viewers never get a permanent link to a video. They ask the API, which checks they may watch it and returns a URL that
stops working after a few hours. The API serves no video bytes: a media front door (nginx locally, a CDN in the cloud)
checks the signature on every request.

## The flow
```
player ──GET /v1/playback/{titleOrEpisodeId}──▶ API: logged in? published? entitled?  ──▶ signed manifest URL (+ subtitles)
player ──GET {base}/hls/{token}/{expires}/{assetId}/master.m3u8──▶ nginx: token valid? not expired? ──▶ file from storage
player ──GET .../360p/index.m3u8, .../360p/seg_00001.m4s (same prefix, inherited from the playlist)──▶ nginx ... (206 for ranges)
```
`GET /v1/playback/{id}` takes a movie's title id or an episode id and returns `manifest_url`, `expires_at`, `duration_seconds`,
signed `subtitles[]`, and the public `poster_url` and `thumbnails_url` (hints: a video can lack them). It is `no-store` and rate limited.

## Why the token is in the path
A player resolves the playlist's relative URLs (`360p/index.m3u8`, `seg_00001.m4s`) against the playlist URL. A token in the
**query string** would be lost; a token in the **path prefix** is inherited by every playlist, segment and subtitle for free.
The token covers the expiry and the asset id, so one URL unlocks that asset (and nothing else) until it expires.

`token = base64url( md5( "<expires><assetId> <PLAYBACK_SIGNING_KEY>" ) )`, `expires` a Unix timestamp. This is exactly nginx's
`secure_link_md5`; the secret is a suffix, so length-extension attacks do not apply. The Go signer (`internal/playback/signer.go`)
is tested against an independent computation of the same value.

## What nginx does (`infra/nginx/templates/default.conf.template`)
| Request | Answer |
|---|---|
| valid token, not expired | the file, with `Range` support (206), cached 5 min (playlists) or a year (segments) |
| wrong, altered or reused-for-another-asset token | **403** |
| right token, past `expires` | **410**: the player asks the API for a fresh URL |
| directory, listing, odd extension, `..` | **403** (only `.m3u8 .m4s .mp4 .vtt .ts .aac` files of the asset match) |
| unsigned `/hls/...` | **403** |
nginx reads the private `media-hls` bucket through SeaweedFS's file API (port 8888), which has no authentication, so that port is
**never published to the host**: only nginx, on the Docker network, can reach it, and only through the signed location. The query
string is not forwarded, so filer options such as `?metadata=true` cannot be used. Anonymous S3 access to `media-hls` is still 403.

## Access rules
`playback.Entitlements.CanWatch(user, title)` is the hook Phase 4 replaces with real rules (subscription, rental, free window).
Today `PublishedOnly` lets any signed-in viewer watch published titles. Owners and admins can preview their own unpublished videos.
A video that is not published is reported as 404 (`not_playable`) to everyone else, so its existence is not revealed;
unpublishing a title cuts viewers off immediately, apart from signed URLs they already hold (valid until `expires_at`, 4 h by default).

## Settings
`PLAYBACK_SIGNING_KEY` (32+ characters, no spaces, quotes or `$`; shared by the API and nginx), `PLAYBACK_BASE_URL` (what the browser
reaches), `PLAYBACK_TOKEN_TTL_HOURS` (2 to 6, default 4). **Rotating the key:** change it in `.env`, then
`docker compose up -d --force-recreate api nginx`; URLs handed out before are refused (403) and players recover by asking the API again.

## In the cloud (the same URL shape)
The API only builds a URL; the front door only verifies it. To move to a CDN, give it the same check:
- **Cloudflare:** a WAF rule using `is_timed_hmac_valid_v0`, or a Worker that recomputes the token. Implement an HMAC-SHA256 `Signer`
  that produces the same `/hls/{token}/{expires}/{assetId}/{file}` path (the Signer is a small type behind `playback.Service`).
- **Bunny.net:** token authentication with a directory token (`token_path`), which is also path-scoped.
- **CloudFront:** signed cookies or a policy covering `/hls/*/{assetId}/*`.
The origin (R2/S3) stays private; the CDN holds the credentials, not the browser.

## Trying it
```
make up && make migrate-up
make dev-creator EMAIL=you@example.com                   # a draft movie you own
make upload-test ARGS="--email you@example.com --password '...' --title <id> --file sample.mp4"   # wait until ready
make playback-check ARGS="--email you@example.com --password '...' --id <id>"                     # signed URL + attack checks
```
`playback-check` prints the signed URL and proves: the manifest, a playlist and a ranged segment load; a tampered token, a token for
another asset and a directory listing are refused; an expired URL gets 410. To watch it in a browser, open `docs/player.html`, paste the
`manifest_url`, and press Play (Chrome and Firefox use hls.js, Safari plays HLS natively).
