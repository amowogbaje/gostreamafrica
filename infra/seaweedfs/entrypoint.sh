#!/bin/sh
# Wrapper around the official SeaweedFS image entrypoint.
# 1. Renders the S3 identities file from environment variables, so credentials come from .env
#    and never live in git. WITHOUT a config file SeaweedFS accepts ANY access key.
# 2. Hands over to the image's own /entrypoint.sh (it fixes /data ownership and drops to the
#    unprivileged "seaweed" user).
#
# Identities:
#   anonymous      Read on media-public ONLY (posters, thumbnails). Everything else needs a signature.
#   streamafrica   the application (API + worker): full access with S3_ACCESS_KEY / S3_SECRET_KEY.
set -eu
: "${S3_ACCESS_KEY:?S3_ACCESS_KEY is required}"
: "${S3_SECRET_KEY:?S3_SECRET_KEY is required}"
: "${S3_BUCKET_PUBLIC:?S3_BUCKET_PUBLIC is required}"

cat > /tmp/s3.json <<JSON
{
  "identities": [
    { "name": "anonymous", "actions": ["Read:${S3_BUCKET_PUBLIC}"] },
    {
      "name": "streamafrica",
      "credentials": [ { "accessKey": "${S3_ACCESS_KEY}", "secretKey": "${S3_SECRET_KEY}" } ],
      "actions": ["Admin", "Read", "List", "Tagging", "Write"]
    }
  ]
}
JSON
chmod 644 /tmp/s3.json

exec /entrypoint.sh "$@"
