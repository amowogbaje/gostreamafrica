# Applying Phase 1 (steps 1, 2 and 3)

This zip is an overlay: it adds and replaces files and never touches your `.env`, `go.mod`, `go.sum`,
`Dockerfile.dev` or the web and worker folders. Delete this file when you are done.

## 1. Commit first, so `git diff` shows exactly what changes
```bash
git add -A && git commit -m "before phase 1"
```

## 2. Remove files this version replaces by new names
```bash
rm -f api/internal/auth/handler_test.go api/internal/auth/store_pg_test.go docs/api/openapi.yaml
```
(The old auth tests would clash with the new ones; the spec moved to `docs/openapi.yaml`.)

## 3. Unzip over the repo
```bash
unzip -o streamafrica-phase1-overlay.zip
```
`docker-compose.yml` and `.env.example` are replaced. If you had personal edits in the compose file, re-apply them
from `git diff docker-compose.yml` (ports belong in `.env`, not in the compose file).

## 4. One-character fix in your Makefile
`make help` prints file names once `infra/make/ci.mk` is included. Change `grep -E` to `grep -hE` in the `help` target:
```bash
sed -i 's/@grep -E /@grep -hE /' Makefile
grep -n "include infra/make/ci.mk" Makefile || echo "-include infra/make/ci.mk" >> Makefile
```

## 5. New settings (keeps every value you already set)
```bash
make env-upgrade        # appends the new variables to .env, e.g. AUTH_JWT_SECRET and SMTP_*
```
For anything but local development, generate a real secret: `openssl rand -base64 48`.

## 6. Go dependencies
```bash
docker compose run --rm --no-deps api sh -c \
  "go get github.com/golang-jwt/jwt/v5@v5.2.1 github.com/getkin/kin-openapi@v0.128.0 \
          golang.org/x/crypto@v0.31.0 golang.org/x/sync@v0.10.0 && go mod tidy"
```
Commit the updated `api/go.mod` and `api/go.sum`.

## 7. Start and migrate
```bash
make up
make migrate-up          # applies migrations 000004 to 000012 (expect 4/u ... 12/u)
```

## 8. Verify
```bash
make fmt && make lint
make test                # the stack must be running: includes integration and OpenAPI contract tests
make test-unit           # same suite without needing the database (integration tests skip)
make seed                # 12 fictional sample titles (dev only)
curl -s localhost:8088/api/v1/genres
curl -s "localhost:8088/api/v1/titles?limit=3"
make seed-bulk && make explain     # 20,000 titles: shows indexes being used. make seed-bulk-clean removes them
```
More requests, with the JSON they return, are in `docs/api-examples.md`.

## What could go wrong
- **API exits at start with `AUTH_JWT_SECRET is required`:** run `make env-upgrade`, then `docker compose up -d api`.
- **API unhealthy, log says `missing go.sum entry`:** run step 6.
- **`make test` says `connection refused`:** the stack is not running; `make up` first (or use `make test-unit`).
- **Contract test cannot find the spec:** the compose file must mount `./docs:/docs:ro` and set `OPENAPI_SPEC`; step 3 does this.
- **Web login cookie not sent:** `AUTH_REFRESH_COOKIE_PATH` must match the URL prefix the browser uses (`/api/v1/auth` behind nginx).
- **Refresh answers 403 `forbidden_origin`:** add the browser's origin to `AUTH_ALLOWED_ORIGINS`.
- **Migration 000005 or later fails:** send me `docker compose logs` for `migrate`; do not edit applied migrations.
