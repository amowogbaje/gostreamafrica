# Included from the root Makefile with: -include infra/make/ci.mk
.PHONY: docker-build actionlint env-upgrade test-unit seed seed-bulk seed-bulk-clean explain docs docs-sync s3-check storage-ls dev-creator upload-test

docker-build: ## Build the three production images locally (same as CI)
	docker build -t streamafrica-api:local ./api
	docker build -t streamafrica-worker:local ./worker
	docker build -t streamafrica-web:local ./web

actionlint: ## Lint the GitHub workflow files
	docker run --rm -v "$(CURDIR)":/repo -w /repo rhysd/actionlint:1.7.7 -color

env-upgrade: ## Add variables that are new in .env.example to your .env (never changes existing values)
	@sh infra/scripts/env-upgrade.sh

test-unit: .env ## Go tests that need no database or Redis (the stack can be down)
	$(COMPOSE) run --rm --no-deps -e TEST_DATABASE_URL= -e TEST_REDIS_URL= api sh -c "go mod tidy && go test -count=1 ./..."

# Sample data helpers. They run psql inside the postgres container and refuse unless APP_ENV=local.
PSQL = $(COMPOSE) exec -T postgres sh -c 'psql -q -U "$$POSTGRES_USER" -d "$$POSTGRES_DB" -v ON_ERROR_STOP=1 -v sample_ok=1'
LOCAL_ONLY = @grep -qE '^APP_ENV=local' .env || { echo "refusing: APP_ENV must be local in .env"; exit 1; }

seed: .env ## Load 12 FICTIONAL sample titles (dev only; safe to repeat)
	$(LOCAL_ONLY)
	$(PSQL) < api/seeds/dev_sample_data.sql

seed-bulk: .env ## Load 20,000 generated titles and activity so EXPLAIN shows realistic plans (dev only)
	$(LOCAL_ONLY)
	$(PSQL) < api/seeds/dev_bulk_perf.sql

seed-bulk-clean: .env ## Remove the generated bulk rows
	$(LOCAL_ONLY)
	echo "DELETE FROM titles WHERE slug LIKE 'bulk-%'; DELETE FROM users WHERE email LIKE 'bulk-%@seed.invalid';" | $(PSQL)

explain: .env ## Show query plans for the main access patterns (run make seed-bulk first)
	$(PSQL) < api/seeds/explain_queries.sql

docs: ## Show where the interactive API docs (Swagger UI) are
	@set -a; [ -f .env ] && . ./.env; set +a; \
	echo "Swagger UI : http://localhost:$${NGINX_HOST_PORT:-8088}/api/docs/    (direct: http://localhost:$${API_HOST_PORT:-8080}/docs/)"; \
	echo "Raw spec   : http://localhost:$${NGINX_HOST_PORT:-8088}/api/docs/openapi.yaml"

docs-sync: ## After editing docs/openapi.yaml: refresh the copy embedded in the API binary
	cp docs/openapi.yaml api/internal/apidocs/openapi.yaml
	@echo "synced. Rebuild the API (it restarts itself in dev) and run: make test"

s3-check: ## Prove the object store supports multipart, presigned URLs, CORS, ranges (needs 'make up')
	@sh infra/scripts/s3-check.sh

storage-ls: ## List buckets and their sizes in the object store
	@docker compose exec -T seaweedfs sh -c 'echo "s3.bucket.list" | weed shell -master=localhost:9333 2>/dev/null | grep size:'

dev-creator: .env ## Make an account a creator and print a draft movie id to upload to (EMAIL=you@example.com)
	$(LOCAL_ONLY)
	@test -n "$(EMAIL)" || { echo "usage: make dev-creator EMAIL=you@example.com   (register that account first)"; exit 1; }
	$(COMPOSE) exec -T postgres sh -c 'psql -q -U "$$POSTGRES_USER" -d "$$POSTGRES_DB" -v ON_ERROR_STOP=1 -v email="$(EMAIL)"' < api/seeds/dev_upload_creator.sql 2>&1 | sed -n 's/^.*NOTICE:  //p'

upload-test: ## Upload a file through the API like a browser would (see docs/upload-flow.md): make upload-test ARGS="--email .. --password .. --title .. --file video.mp4"
	@python3 infra/scripts/upload_client.py $(ARGS)
