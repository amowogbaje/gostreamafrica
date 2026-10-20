# Adding the interactive API docs (Swagger UI inside the API)

Apply on top of the Phase 1 overlay. Nothing new runs: the API serves the page itself, from files embedded in
its binary. Delete this file when done.

```bash
git add -A && git commit -m "before docs ui"
unzip -o streamafrica-docs-ui-overlay.zip

# one new Go dependency (a YAML reader, used to serve the spec as JSON)
docker compose run --rm --no-deps api sh -c "go get gopkg.in/yaml.v3@v3.0.1 && go mod tidy"

docker compose up -d --build api
make docs                      # prints the URLs
```
Open **http://localhost:8088/api/docs/** (or http://localhost:8080/docs/ without nginx).

## Use it
1. Expand **POST /v1/auth/register** (or **/v1/auth/login**) > **Try it out** > **Execute**. Copy `access_token` from the response.
2. Click **Authorize** (top right), paste the token (no `Bearer ` prefix), **Authorize**, **Close**.
3. Try any endpoint. Operations with a padlock need the token. Tokens last 15 minutes: log in again on `token_expired`.
4. Verification and reset emails arrive in Mailpit, http://localhost:8025.
5. The raw spec for Postman or Bruno: http://localhost:8088/api/docs/openapi.yaml

## Settings
- `API_DOCS_ENABLED` is on by default only when `APP_ENV=local`. Anywhere else the pages answer 404 unless you set it to `true`.
- To override from `.env`, add this line under `environment:` of the `api` service in `docker-compose.yml`:
  `      API_DOCS_ENABLED: ${API_DOCS_ENABLED:-}`

## Editing the API contract from now on
`docs/openapi.yaml` is the file you edit. The API embeds a copy; a test fails when they differ:
```bash
$EDITOR docs/openapi.yaml
make docs-sync && make test
```

## Check it
```bash
make test        # includes the docs tests and the contract tests
curl -s -o /dev/null -w "%{http_code}\n" localhost:8088/api/docs/          # 200
curl -s localhost:8088/api/docs/openapi.json | head -c 120; echo
```

## What could go wrong
- **Page is blank:** open the browser console. The page loads only from its own origin, so use http://localhost:8088/api/docs/
  or http://localhost:8080/docs/, not a different hostname or a proxy that rewrites paths.
- **`go: missing go.sum entry` or the API does not start:** rerun the `go get ... && go mod tidy` command.
- **"Try it out" returns 401:** you did not Authorize, or the token expired (15 minutes).
- **`make test` says `docs/openapi.yaml and ...apidocs/openapi.yaml differ`:** run `make docs-sync`.
