# Included from the root Makefile with: -include infra/make/ci.mk
.PHONY: docker-build actionlint env-upgrade test-unit seed seed-bulk seed-bulk-clean explain

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
