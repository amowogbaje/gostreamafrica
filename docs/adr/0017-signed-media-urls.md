# ADR-0017: Signed media URLs, token in the path, checked by nginx secure_link
Status: Accepted

## Context
Videos live in a private bucket. Players fetch a master playlist, then rendition playlists, then hundreds of segments with relative
URLs, and send `Range` requests. Access must expire, must not require the API to proxy video, and must map to a CDN later.
nginx cannot sign S3 requests, so it cannot read a private bucket on its own (ADR-0014's open item).

## Decision
- `GET /v1/playback/{id}` authorizes (`Entitlements` hook, published-only for now, owner/admin preview) and returns
  `{base}/hls/{token}/{expires}/{assetId}/master.m3u8`, valid 4 hours (2 to 6 configurable).
- **Token in the path, scoped to the asset:** relative URLs inherit the path prefix, so one token covers the playlist, renditions,
  segments and subtitles of that asset and nothing else. A query-string token would be dropped by the player's URL resolution.
- **Checked by nginx's native `secure_link` module:** `base64url(md5("<expires><assetId> <key>"))`, no third-party module. The key is a
  suffix, so MD5 length extension does not apply. 403 for a bad token, 410 for an expired one (the player re-asks the API).
- **Origin access:** nginx proxies the signed location to SeaweedFS's file API on the internal network (never published to the host),
  with no query string and a strict filename pattern. Anonymous S3 access to `media-hls` stays denied.
- The signer is a small type; a CDN gets the same URL shape with its own HMAC check (docs/playback.md).

## Consequences
- A leaked URL works for everyone until it expires (like every CDN signed URL). Mitigation: short TTL; per-viewer or per-IP binding can be added later
  by including the viewer in the signed string.
- Unpublishing stops new URLs at once; URLs already issued live until expiry.
- MD5 is used only because nginx's module offers nothing else natively; the cloud edge can use HMAC-SHA256 without changing clients.
- Rotating the key invalidates outstanding URLs; players recover by asking the API again.
