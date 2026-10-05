#!/bin/sh
# One-shot: create buckets idempotently. Runs inside the minio/mc image.
set -eu

i=0
until mc alias set local "$S3_ENDPOINT" "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 30 ]; then echo "minio not reachable at $S3_ENDPOINT" >&2; exit 1; fi
  sleep 1
done

for b in "$S3_BUCKET_RAW" "$S3_BUCKET_HLS" "$S3_BUCKET_PUBLIC"; do
  mc mb --ignore-existing "local/$b"
done

# raw uploads and HLS are private; only public assets (posters etc.) are anonymous-read.
mc anonymous set none "local/$S3_BUCKET_RAW"
mc anonymous set none "local/$S3_BUCKET_HLS"
mc anonymous set download "local/$S3_BUCKET_PUBLIC"

echo "buckets ready: $S3_BUCKET_RAW $S3_BUCKET_HLS $S3_BUCKET_PUBLIC"
