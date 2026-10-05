COMPOSE ?= docker compose
GOLANGCI ?= golangci/golangci-lint:v1.64.8
WEB_RUN = $(COMPOSE) run --rm --no-deps web sh -c

.DEFAULT_GOAL := help
.PHONY: help up down logs ps migrate-up migrate-down migrate-create tidy test lint fmt smoke clean doctor

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'

.env:
	cp .env.example .env
	@echo "Created .env from .env.example"

up: .env ## Build and start the whole stack in the background
	$(COMPOSE) up -d --build
	@echo "Started. Run 'make ps' to watch health, 'make smoke' to verify."

down: ## Stop the stack (keeps volumes)
	$(COMPOSE) down

clean: ## Stop the stack and DELETE all volumes (db, redis, minio data)
	$(COMPOSE) down -v

logs: ## Tail logs (usage: make logs s=api)
	$(COMPOSE) logs -f --tail=100 $(s)

ps: ## Show service status and health
	$(COMPOSE) ps

migrate-up: .env ## Apply all pending migrations
	$(COMPOSE) --profile tools run --rm migrate up

migrate-down: .env ## Roll back ONE migration
	$(COMPOSE) --profile tools run --rm migrate down 1

test: .env ## Run Go tests (api, worker) and web typecheck
	$(COMPOSE) run --rm --no-deps api sh -c "go mod tidy && go test -count=1 ./..."
	$(COMPOSE) run --rm --no-deps worker go test -count=1 ./...
	$(WEB_RUN) "npm install --no-audit --no-fund && npm run typecheck"

lint: .env ## Run golangci-lint (api, worker) and eslint (web)
	docker run --rm -v "$(CURDIR)/api:/app" -v "$(CURDIR)/.golangci.yml:/cfg/.golangci.yml:ro" -w /app $(GOLANGCI) golangci-lint run -c /cfg/.golangci.yml ./...
	docker run --rm -v "$(CURDIR)/worker:/app" -v "$(CURDIR)/.golangci.yml:/cfg/.golangci.yml:ro" -w /app $(GOLANGCI) golangci-lint run -c /cfg/.golangci.yml ./...
	$(WEB_RUN) "npm install --no-audit --no-fund && npm run lint"

fmt: .env ## gofmt api and worker
	$(COMPOSE) run --rm --no-deps api gofmt -w .
	$(COMPOSE) run --rm --no-deps worker gofmt -w .

smoke: ## End-to-end check of every endpoint (needs 'make up' first)
	@sh infra/scripts/smoke.sh

tidy: .env ## go mod tidy for api and worker (generates go.sum)
	$(COMPOSE) run --rm --no-deps api go mod tidy
	$(COMPOSE) run --rm --no-deps worker go mod tidy

migrate-create: .env ## Create a migration pair (usage: make migrate-create name=add_users)
	@test -n "$(name)" || (echo "usage: make migrate-create name=add_users" && exit 1)
	$(COMPOSE) --profile tools run --rm migrate create -ext sql -dir /migrations -seq $(name)

doctor: ## Print the facts you need when something is broken
	@sh infra/scripts/doctor.sh
