#!/usr/bin/env sh
# Prints the facts needed to debug a broken stack. Safe to run any time.
cd "$(dirname "$0")/../.."
if [ -f .env ]; then set -a; . ./.env; set +a; fi
echo "== docker / compose versions"; docker --version; docker compose version
echo; echo "== memory given to Docker"; docker info --format '{{.MemTotal}} bytes' 2>/dev/null
echo; echo "== disk"; docker system df 2>/dev/null
echo; echo "== compose config valid?"; docker compose config -q && echo yes
echo; echo "== images this stack needs"; docker compose config --images
echo; echo "== services"; docker compose ps -a
echo; echo "== unhealthy / exited, last 20 log lines each"
for s in $(docker compose ps -a --format '{{.Service}} {{.Health}} {{.State}}' | awk '$2=="unhealthy" || $3=="exited" || $3=="restarting" {print $1}'); do
  echo "---- $s"; docker compose logs --tail=20 "$s"
done
echo; echo "== ports in use on this host"
for p in "${WEB_HOST_PORT:-3000}" "${POSTGRES_HOST_PORT:-5432}" "${REDIS_HOST_PORT:-6379}" "${MAILPIT_UI_HOST_PORT:-8025}" "${MAILPIT_SMTP_HOST_PORT:-1025}" "${API_HOST_PORT:-8080}" "${NGINX_HOST_PORT:-8088}" "${MINIO_API_HOST_PORT:-9000}" "${MINIO_CONSOLE_HOST_PORT:-9001}"; do
  (ss -ltn 2>/dev/null || netstat -an 2>/dev/null) | grep -q "[:.]$p " && echo "port $p is IN USE"
done
exit 0
