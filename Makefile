# Every target here is also what CI runs. A command that only exists in a
# workflow file is a command nobody can reproduce when it fails.

SHELL := /bin/sh
COMPOSE := docker compose -f deploy/compose/compose.yaml --env-file .env
# The deployed stack, behind Traefik. Same environment file, because the
# production compose file reads only variables whose value is a deployment
# decision: the addresses of the rendering side are service names written into
# the file itself, so there is no local value it can inherit and get wrong.
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
# The deployed stack. Every target below runs on the host that serves it, from
# a checkout of this repository, against a Traefik that is already up and a
# network it already created.
# ─────────────────────────────────────────────────────────────────────────────

# Run this first on a new host, and after every change to .env. It resolves the
# whole file and names the first variable that has no value, which is a better
# place to find out than halfway through a deployment that has already stopped
# the previous containers.
.PHONY: prod-config
prod-config: .env no-stray-env ## Render the production compose file with the environment resolved
	$(COMPOSE_PROD) config

# The version reaches the binaries through a build argument, so `recon -version`
# and the logs name a commit rather than "dev". A checkout with no tag still
# gives a short SHA, and -dirty is worth seeing on a deployed host.
.PHONY: prod-up
prod-up: .env no-stray-env ## Build and start the deployed stack behind Traefik
	VERSION="$$(git describe --tags --always --dirty)" $(COMPOSE_PROD) up -d --build

.PHONY: prod-down
prod-down: ## Stop the deployed stack, keeping the data
	$(COMPOSE_PROD) down

# Everything except the Certificate Transparency feed, which narrates every
# certificate the public logs publish. That is thousands of lines a minute and
# none of them are about this deployment, so it buries the one service that was
# about to say something.
#
# Written as an exclusion rather than as a list of what to follow, so a service
# added to the compose file shows up here without anybody remembering to add it.
# The feed is still there when it is the thing in question:
#
#   make prod-logs SERVICE=certstream
.PHONY: prod-logs
prod-logs: ## Follow the deployed stack's logs, minus the certificate feed
	@if [ -n "$(SERVICE)" ]; then \
		$(COMPOSE_PROD) logs -f $(SERVICE); \
	else \
		$(COMPOSE_PROD) logs -f $$($(COMPOSE_PROD) config --services | grep -vx certstream); \
	fi

# Only the images this stack does not build. `up --build` rebuilds ours from
# the checkout and leaves these where they are, so without this a deployment
# keeps whatever was pulled the first time, for as long as the tag says latest.
.PHONY: prod-pull
prod-pull: ## Refresh the images the deployed stack does not build
	$(COMPOSE_PROD) pull postgres chrome-1 certstream fingerprinter

# Normally redundant: the control plane waits on the migration, the roles and
# the seed, so `prod-up` has already run all three. It exists for the deployment
# that only has migrations to apply.
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

# There is one environment file and it lives at the root, because that is what
# --env-file above names. Docker compose also reads a .env sitting beside the
# compose file, and it wins when compose is invoked from that directory, so a
# copy left there is a second configuration that shadows this one on some
# invocations and not others. That is not a tidiness rule: it costs an
# afternoon, because both files work and they disagree.
.PHONY: no-stray-env
no-stray-env:
	@if [ -f deploy/compose/.env ]; then \
		echo "deploy/compose/.env shadows the root .env whenever compose runs from that"; \
		echo "directory, and the two will disagree. There is one env file, at the root."; \
		echo "  rm deploy/compose/.env"; \
		exit 1; \
	fi
