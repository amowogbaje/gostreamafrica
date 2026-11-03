#!/usr/bin/env sh
# Proves the object store supports everything Phase 2 needs. Run after `make up`.
#   Part 1 (inside Docker): 13 checks with a real S3 SDK (multipart, presigned URLs, anonymous
#          rules, ranges, CORS, listing, metadata).
#   Part 2 (from your machine, like a browser): upload through a presigned URL on the PUBLIC
#          endpoint with an Origin header, then check CORS headers and anonymous access.
set -eu
cd "$(dirname "$0")/../.."
if [ -f .env ]; then set -a; . ./.env; set +a; fi

S3=${S3_HOST_PORT:-8333}
PUB=${S3_BUCKET_PUBLIC:-media-public}; HLS=${S3_BUCKET_HLS:-media-hls}
ORIGIN=${S3_ALLOWED_ORIGINS%%,*}
BAD=http://not-allowed.example
fail=0
ok()  { printf 'ok    %s\n' "$1"; }
bad() { printf 'FAIL  %s\n' "$1"; fail=1; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@" 2>/dev/null || echo 000; }

echo "== Part 1: SDK checks inside Docker (the first run downloads a Python image, ~1 min)"
docker compose --profile tools run --rm -T s3check check || fail=1

echo
echo "== Part 2: browser path from this machine (http://localhost:$S3, Origin: $ORIGIN)"
lines=$(docker compose --profile tools run --rm -T s3check presign)
PUT_URL=$(printf '%s\n' "$lines" | sed -n 's/^PUT_URL=//p' | tr -d '\r')
GET_URL=$(printf '%s\n' "$lines" | sed -n 's/^GET_URL=//p' | tr -d '\r')
KEY=$(printf '%s\n' "$lines" | sed -n 's/^KEY=//p' | tr -d '\r')
[ -n "$PUT_URL" ] && [ -n "$GET_URL" ] || { echo "could not get presigned URLs"; exit 1; }

hdr=$(mktemp); trap 'rm -f "$hdr"' EXIT
c=$(curl -s -o /dev/null -D "$hdr" -w '%{http_code}' -X PUT --data-binary 'streamafrica-browser-upload' \
     -H 'Content-Type: video/mp4' -H "Origin: $ORIGIN" "$PUT_URL" 2>/dev/null || echo 000)
[ "$c" = 200 ] && ok "presigned PUT from the host works" || bad "presigned PUT from the host -> HTTP $c (is S3_PUBLIC_ENDPOINT = http://localhost:$S3 ?)"
grep -qi "^access-control-allow-origin: $ORIGIN" "$hdr" && ok "PUT response allows origin $ORIGIN" || bad "PUT response has no Access-Control-Allow-Origin for $ORIGIN"
grep -i '^access-control-expose-headers:' "$hdr" | grep -qi etag && ok "ETag is exposed to the browser (needed for multipart)" || bad "ETag not exposed in Access-Control-Expose-Headers"
grep -qi '^etag:' "$hdr" && ok "PUT response carries an ETag" || bad "PUT response has no ETag"

[ "$(curl -s "$GET_URL")" = "streamafrica-browser-upload" ] && ok "presigned GET returns the uploaded bytes" || bad "presigned GET did not return the uploaded bytes"
c=$(code -X OPTIONS -H "Origin: $BAD" -H 'Access-Control-Request-Method: PUT' "$PUT_URL")
acao=$(curl -s -o /dev/null -D - -X OPTIONS -H "Origin: $BAD" -H 'Access-Control-Request-Method: PUT' "$PUT_URL" 2>/dev/null | tr -d '\r' | grep -i '^access-control-allow-origin:' || true)
[ -z "$acao" ] && ok "a disallowed origin gets no CORS approval (HTTP $c)" || bad "disallowed origin was approved: $acao"

docker compose --profile tools run --rm -T s3check seed >/dev/null
[ "$(code "http://localhost:$S3/$PUB/_smoke/test.txt")" = 200 ] && ok "anonymous GET on $PUB is 200" || bad "anonymous GET on $PUB should be 200"
[ "$(code "http://localhost:$S3/$HLS/_smoke/test.txt")" = 403 ] && ok "anonymous GET on $HLS is 403" || bad "anonymous GET on $HLS should be 403"
[ "$(code -X PUT --data x "http://localhost:$S3/$PUB/_smoke/evil.txt")" = 403 ] && ok "anonymous PUT on $PUB is 403" || bad "anonymous PUT on $PUB should be 403"

docker compose --profile tools run --rm -T -e CLEAN_PREFIX="${KEY%browser.bin}" s3check cleanup >/dev/null 2>&1 || true

echo
[ "$fail" -eq 0 ] && echo "S3 CHECK: ALL GOOD" || { echo "S3 CHECK: FAILED (see lines marked FAIL; 'make logs s=seaweedfs')"; exit 1; }
