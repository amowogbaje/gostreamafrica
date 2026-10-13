#!/usr/bin/env sh
# Adds variables that exist in .env.example but are missing from .env.
# It never changes or removes a value you already set, so your local edits are safe.
set -eu
cd "$(dirname "$0")/../.."
if [ ! -f .env ]; then
  cp .env.example .env
  echo "created .env from .env.example"
  exit 0
fi
added=0
tmp=$(mktemp)
grep -E '^[A-Z][A-Z0-9_]*=' .env.example > "$tmp"
while IFS= read -r line; do
  key=${line%%=*}
  if ! grep -qE "^${key}=" .env; then
    printf '%s\n' "$line" >> .env
    echo "added  $line"
    added=$((added + 1))
  fi
done < "$tmp"
# informational: variables in .env that .env.example no longer documents
grep -E '^[A-Z][A-Z0-9_]*=' .env | cut -d= -f1 | while IFS= read -r key; do
  grep -qE "^${key}=" "$tmp" || echo "note   $key is in .env but not in .env.example (unused or yours)"
done
rm -f "$tmp"
echo "done: $added variable(s) added"
