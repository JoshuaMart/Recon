# Every target here is also what CI runs. A command that only exists in a
# workflow file is a command nobody can reproduce when it fails.

SHELL := /bin/sh
COMPOSE := docker compose -f deploy/compose/compose.yaml --env-file .env
GO ?= go

.DEFAULT_GOAL := help

.PHONY: help
help: ## List the targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Compile the binaries into bin/
	$(GO) build -trimpath -o bin/ ./cmd/...

.PHONY: fmt
fmt: ## Format
	golangci-lint fmt ./...

.PHONY: lint
lint: ## Vet and lint
	$(GO) vet ./...
	golangci-lint run ./...

.PHONY: test
test: ## Unit tests, with the race detector
	$(GO) test -race ./...

.PHONY: test-integration
test-integration: ## Tests that need a container
	$(GO) test -tags integration -count=1 ./...

.PHONY: check
check: lint test ## What a pull request has to pass

.PHONY: up
up: .env ## Start the local stack
	$(COMPOSE) up -d --build

.PHONY: down
down: ## Stop it, keeping the data
	$(COMPOSE) down

.PHONY: clean
clean: ## Stop it and delete the data and the archive
	$(COMPOSE) down -v
	rm -rf var/archive

.PHONY: logs
logs: ## Follow the stack's logs
	$(COMPOSE) logs -f

.PHONY: migrate
migrate: ## Apply pending migrations against the running stack
	$(COMPOSE) run --rm migrate up

.PHONY: verify-isolation
verify-isolation: ## Prove the rendering network reaches neither the database nor the API
	@set -a; . ./.env; set +a; deploy/compose/verify-isolation.sh

.PHONY: hooks
hooks: ## Install the git hooks, which is what makes the secret scan local
	git config core.hooksPath .githooks
	@echo "hooks installed: $$(git config core.hooksPath)"

# A missing .env is the first thing a clean machine hits, and the compose file's
# own error would name one variable at a time.
.env:
	@echo "No .env. Copy .env.example, fill in the passwords, and run again:"
	@echo "  cp .env.example .env"
	@exit 1
