# Every target here is also what CI runs. A command that only exists in a
# workflow file is a command nobody can reproduce when it fails.

SHELL := /bin/sh
COMPOSE := docker compose -f deploy/compose/compose.yaml --env-file .env
# The deployed stack, behind Traefik. Same environment file: the production compose file reads
# only variables that are deployment decisions, and the rendering side's addresses are service
# names written into the file, so there is no local value to inherit wrongly.
COMPOSE_PROD := docker compose -f deploy/compose/compose.prod.yaml --env-file .env
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

.PHONY: console
console: ## Type check, lint and test the console
	cd web && pnpm install --frozen-lockfile && pnpm run check && pnpm run lint && pnpm test

.PHONY: bootstrap
bootstrap: ## Create the first organization and print its token
	@if [ -z "$(ORG)" ] || [ -z "$(EMAIL)" ]; then \
		echo "usage: make bootstrap ORG=\"Name\" EMAIL=person@example.com"; \
		exit 1; \
	fi
	$(COMPOSE) run --rm --build recon bootstrap -org "$(ORG)" -email "$(EMAIL)"

.PHONY: check
check: lint test console ## What a pull request has to pass

.PHONY: up
up: .env no-stray-env ## Start the local stack
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

# ─────────────────────────────────────────────────────────────────────────────
# The deployed stack. Every target runs on the host serving it, from a checkout, against a
# Traefik already up and a network it already created.
# ─────────────────────────────────────────────────────────────────────────────

# Run first on a new host and after every .env change. It resolves the whole file and names the
# first variable with no value, better learned here than halfway through a deployment that has
# stopped the previous containers.
.PHONY: prod-config
prod-config: .env no-stray-env ## Render the production compose file with the environment resolved
	$(COMPOSE_PROD) config

# The version reaches the binaries through a build argument, so `recon -version` and the logs
# name a commit, not "dev". An untagged checkout still gives a short SHA; -dirty is worth seeing
# on a deployed host.
.PHONY: prod-up
prod-up: .env no-stray-env ## Build and start the deployed stack behind Traefik
	VERSION="$$(git describe --tags --always --dirty)" $(COMPOSE_PROD) up -d --build

.PHONY: prod-down
prod-down: ## Stop the deployed stack, keeping the data
	$(COMPOSE_PROD) down

# Everything except the CT feed, which narrates every certificate the public logs publish:
# thousands of lines a minute, none about this deployment, burying the service about to say
# something.
#
# An exclusion, not a list to follow, so a service added to compose shows up without anyone
# remembering. The feed is still there when it is the subject:
#
#   make prod-logs SERVICE=certstream
.PHONY: prod-logs
prod-logs: ## Follow the deployed stack's logs, minus the certificate feed
	@if [ -n "$(SERVICE)" ]; then \
		$(COMPOSE_PROD) logs -f $(SERVICE); \
	else \
		$(COMPOSE_PROD) logs -f $$($(COMPOSE_PROD) config --services | grep -vx certstream); \
	fi

# Only images this stack does not build. `up --build` rebuilds ours and leaves these, so
# without this a deployment keeps whatever was first pulled for as long as the tag says latest.
.PHONY: prod-pull
prod-pull: ## Refresh the images the deployed stack does not build
	$(COMPOSE_PROD) pull postgres chrome-1 chrome-2 chrome-3 certstream fingerprinter

# Normally redundant: the control plane waits on the migration, roles and seed, so `prod-up`
# already ran all three. For a deployment that only has migrations to apply.
.PHONY: prod-migrate
prod-migrate: ## Apply pending migrations against the deployed stack
	$(COMPOSE_PROD) run --rm migrate up

.PHONY: prod-bootstrap
prod-bootstrap: ## Create the first organization on the deployed stack and print its token
	@if [ -z "$(ORG)" ] || [ -z "$(EMAIL)" ]; then \
		echo "usage: make prod-bootstrap ORG=\"Name\" EMAIL=person@example.com"; \
		exit 1; \
	fi
	$(COMPOSE_PROD) run --rm --build recon bootstrap -org "$(ORG)" -email "$(EMAIL)"

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

# One environment file, at the root, as --env-file names. Compose also reads a .env beside the
# compose file and it wins when invoked from that directory, so a copy left there is a second
# configuration shadowing this one on some invocations. Not tidiness: both files work and they
# disagree, which costs an afternoon.
.PHONY: no-stray-env
no-stray-env:
	@if [ -f deploy/compose/.env ]; then \
		echo "deploy/compose/.env shadows the root .env whenever compose runs from that"; \
		echo "directory, and the two will disagree. There is one env file, at the root."; \
		echo "  rm deploy/compose/.env"; \
		exit 1; \
	fi
