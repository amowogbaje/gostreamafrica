#!/usr/bin/env sh
# End-to-end check of the local stack. Run after `make up`.
set -eu
cd "$(dirname "$0")/../.."
if [ -f .env ]; then set -a; . ./.env; set +a; fi

WEB=${WEB_HOST_PORT:-3000}; API=${API_HOST_PORT:-8080}; NGX=${NGINX_HOST_PORT:-8088}
MINIO=${MINIO_API_HOST_PORT:-9000}; CONSOLE=${MINIO_CONSOLE_HOST_PORT:-9001}; MAIL=${MAILPIT_UI_HOST_PORT:-8025}
BUCKET=${S3_BUCKET_PUBLIC:-media-public}
fail=0

check() { # name command...
  name=$1; shift; i=0
  until "$@" >/dev/null 2>&1; do
    i=$((i + 1))
    if [ "$i" -ge 30 ]; then printf 'FAIL  %s\n' "$name"; fail=1; return; fi
    sleep 2
  done
  printf 'ok    %s\n' "$name"
}

http() { curl -fsS --max-time 5 "$1"; }

readyz_ok()  { http "http://localhost:$API/readyz" | grep -q '"status":"ok"'; }
nginx_api()  { http "http://localhost:$NGX/api/healthz" | grep -q '"status":"ok"'; }

seed_object() {
  docker compose run --rm --no-deps --entrypoint /bin/sh minio-init -c \
    'mc alias set local "$S3_ENDPOINT" "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null && echo "streamafrica-range-test-0123456789" | mc pipe "local/$S3_BUCKET_PUBLIC/_smoke/test.txt" >/dev/null'
}

range_and_cors() {
  out=$(curl -sS --max-time 5 -D - -o /dev/null -H "Range: bytes=0-4" -H "Origin: http://localhost:3000" \
        "http://localhost:$NGX/public/_smoke/test.txt" | tr -d '\r')
  echo "$out" | head -n1 | grep -q ' 206' &&
  echo "$out" | grep -qi '^content-range: bytes 0-4/' &&
  echo "$out" | grep -qi '^access-control-allow-origin:'
}

hls_private() { [ "$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:$NGX/hls/_smoke/none.m3u8")" = "403" ]; }

check "web        /healthz"             http "http://localhost:$WEB/healthz"
check "api        /healthz"             http "http://localhost:$API/healthz"
check "api        /readyz (pg+redis+s3)" readyz_ok
check "minio      live"                 http "http://localhost:$MINIO/minio/health/live"
check "minio      console"              http "http://localhost:$CONSOLE/"
check "mailpit    ui"                   http "http://localhost:$MAIL/readyz"
check "nginx      /nginx-health"        http "http://localhost:$NGX/nginx-health"
check "nginx      /api -> api"          nginx_api
check "nginx      / -> web"             http "http://localhost:$NGX/"
check "seed       $BUCKET/_smoke/test.txt" seed_object
check "nginx      /public range 206 + CORS" range_and_cors
check "nginx      /hls unsigned is 403"  hls_private

[ "$fail" -eq 0 ] && echo "ALL GOOD" || { echo "SOME CHECKS FAILED (see 'make logs')"; exit 1; }
