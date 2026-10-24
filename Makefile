COMPOSE := docker compose --env-file .env -f deploy/compose/compose.yaml
# testcontainers does not read the Docker context, so hand it the active socket.
DOCKER_HOST ?= $(shell docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null)
export DOCKER_HOST

.DEFAULT_GOAL := help

.PHONY: help
help: ## List the targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

.env:
	@echo "No .env: cp .env.example .env and fill it in" >&2; exit 1

.PHONY: up
up: .env ## Build and start the stack, wait until healthy
	mkdir -p var/backups
	$(COMPOSE) up -d --build --wait

.PHONY: down
down: ## Stop the stack, keep the data
	$(COMPOSE) down

.PHONY: logs
logs: ## Follow the logs
	$(COMPOSE) logs -f

.PHONY: ps
ps: ## Show the services
	$(COMPOSE) ps

.PHONY: migrate-status
migrate-status: ## Show applied migrations
	$(COMPOSE) exec recon /recon migrate status

.PHONY: backup
backup: ## Dump the database now into var/backups
	$(COMPOSE) exec backup /backup.sh once

.PHONY: restore-check
restore-check: ## Restore the latest dump into a scratch database and check it
	@latest=$$(ls -1t var/backups/recon-*.dump 2>/dev/null | head -1); \
	  [ -n "$$latest" ] || { echo "no dump in var/backups, run make backup" >&2; exit 1; }; \
	  $(COMPOSE) exec backup /backup.sh verify /backups/$$(basename $$latest)

.PHONY: restore
restore: ## Restore FILE (a name in var/backups) over the live database
	@[ -n "$(FILE)" ] || { echo "usage: make restore FILE=recon-....dump" >&2; exit 1; }
	$(COMPOSE) stop recon
	$(COMPOSE) exec backup /backup.sh restore /backups/$(FILE)
	$(COMPOSE) start recon

.PHONY: verify-isolation
verify-isolation: ## Check the network isolation of the running stack
	deploy/compose/verify-isolation.sh

.PHONY: test
test: ## Unit tests
	go test -race ./...

.PHONY: test-integration
test-integration: ## Integration tests, needs Docker
	go test -race -tags integration -count=1 ./...

.PHONY: lint
lint: ## go vet and golangci-lint
	go vet ./...
	go vet -tags integration ./...
	golangci-lint run --build-tags integration ./...

.PHONY: sqlc
sqlc: ## Regenerate the query code
	sqlc generate

.PHONY: sqlc-check
sqlc-check: ## Fail if the generated query code is stale
	sqlc diff

.PHONY: docs
docs: ## Serve the design docs
	cd docs && pnpm install && pnpm dev

.PHONY: docs-build
docs-build: ## Build the design docs
	cd docs && pnpm install --frozen-lockfile && pnpm build

.PHONY: ui
ui: ## Build the interface into internal/ui/dist, embedded by the next go build
	cd web && pnpm install --frozen-lockfile && pnpm build
	find internal/ui/dist -mindepth 1 ! -name .gitkeep -delete
	cp -R web/build/. internal/ui/dist/

.PHONY: ui-dev
ui-dev: ## Serve the interface with reload, against the API on localhost:8080
	cd web && pnpm install && pnpm dev

.PHONY: ui-check
ui-check: ## Type check the interface
	cd web && pnpm install --frozen-lockfile && pnpm check

.PHONY: check
check: lint test sqlc-check ui-check ## What CI runs, minus integration and docs
