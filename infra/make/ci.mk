# Included from the root Makefile with: -include infra/make/ci.mk
.PHONY: docker-build actionlint

docker-build: ## Build the three production images locally (same as CI)
	docker build -t streamafrica-api:local ./api
	docker build -t streamafrica-worker:local ./worker
	docker build -t streamafrica-web:local ./web

actionlint: ## Lint the GitHub workflow files
	docker run --rm -v "$(CURDIR)":/repo -w /repo rhysd/actionlint:1.7.7 -color
