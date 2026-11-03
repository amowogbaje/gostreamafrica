#!/bin/sh
# One-shot: create the buckets idempotently over the plain S3 API (SigV4 via curl).
# Runs inside the SeaweedFS image (alpine + curl), so no extra image is needed.
# Access rules live in infra/seaweedfs/entrypoint.sh (anonymous read on media-public only).
set -eu

EMPTY_SHA=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
REGION=${S3_REGION:-us-east-1}

create_bucket() {
  b=$1; i=0
  while :; do
    code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
      --user "$S3_ACCESS_KEY:$S3_SECRET_KEY" --aws-sigv4 "aws:amz:$REGION:s3" \
      -H "x-amz-content-sha256: $EMPTY_SHA" "$S3_ENDPOINT/$b" 2>/dev/null) || code=000
    case "$code" in
      200) echo "created $b"; return 0 ;;
      409) echo "exists  $b"; return 0 ;;
      403) echo "S3 credentials rejected (HTTP 403). S3_ACCESS_KEY/S3_SECRET_KEY changed? SeaweedFS reads them at start: run 'docker compose up -d --force-recreate seaweedfs'." >&2; exit 1 ;;
    esac
    i=$((i + 1))
    if [ "$i" -ge 30 ]; then echo "could not create $b at $S3_ENDPOINT (last HTTP $code)" >&2; exit 1; fi
    sleep 2
  done
}

for b in "$S3_BUCKET_RAW" "$S3_BUCKET_HLS" "$S3_BUCKET_PUBLIC"; do create_bucket "$b"; done
echo "buckets ready: $S3_BUCKET_RAW $S3_BUCKET_HLS $S3_BUCKET_PUBLIC"
