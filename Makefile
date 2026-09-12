# Nexus development Makefile.
#
# `help` is the default target because a Makefile whose default action is a
# surprise build is a Makefile people stop using.

SHELL := /bin/bash
.DEFAULT_GOAL := help

COMPOSE       ?= docker compose -f deploy/docker-compose.yml
GO            ?= go
PROTOC        ?= protoc
# protoc plugins install into GOBIN, which is not on PATH in every shell.
GOBIN         ?= $(shell $(GO) env GOPATH)/bin
NAMESPACE     ?= default

# The local dev database from CONTRACT.md. Compose publishes its own on 55433 so
# the two can coexist; override DATABASE_URL to point migrate-up at either.
DATABASE_URL  ?= postgres://postgres:test@localhost:55432/nexus?sslmode=disable
MIGRATIONS    := services/migrations

.PHONY: help proto build test lint migrate-up migrate-down up down logs ps \
        ui-dev ui-build ui-test seed submit-example clean

help: ## Show this help
	@echo "Nexus — make targets"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | sort \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Common overrides: NAMESPACE=$(NAMESPACE) DATABASE_URL=<dsn>"

# ── Codegen ───────────────────────────────────────────────────────────────
proto: ## Regenerate Go bindings from proto/nexus/v1/*.proto
	@command -v $(PROTOC) >/dev/null || { echo "protoc not found on PATH"; exit 1; }
	@test -x "$(GOBIN)/protoc-gen-go" || { echo "protoc-gen-go not in $(GOBIN)"; exit 1; }
	@test -x "$(GOBIN)/protoc-gen-go-grpc" || { echo "protoc-gen-go-grpc not in $(GOBIN)"; exit 1; }
	PATH="$(GOBIN):$$PATH" $(PROTOC) \
	  --proto_path=proto \
	  --go_out=paths=source_relative:gen \
	  --go-grpc_out=paths=source_relative:gen \
	  proto/nexus/v1/*.proto
	@echo "proto: regenerated into gen/nexus/v1"

# ── Go ────────────────────────────────────────────────────────────────────
build: ## Build all Go packages
	$(GO) build ./...

test: ## Run Go tests and UI tests
	$(GO) test ./...
	$(MAKE) ui-test

lint: ## go vet over the whole module
	$(GO) vet ./...

# ── Migrations ────────────────────────────────────────────────────────────
# Applied by a job, never by a service at startup (ADR-0011 rule 4). These
# targets are the local equivalent of that job.
migrate-up: ## Apply all *.up.sql migrations to DATABASE_URL
	@command -v psql >/dev/null || { echo "psql not found; use 'make up' to run the compose migration job instead"; exit 1; }
	@set -euo pipefail; \
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -q -c \
	  "CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())"; \
	for f in $(MIGRATIONS)/*.up.sql; do \
	  v=$$(basename "$$f" .up.sql); \
	  if [ "$$(psql "$(DATABASE_URL)" -tAc "SELECT 1 FROM schema_migrations WHERE version='$$v'")" = "1" ]; then \
	    echo "migrate-up: $$v already applied"; continue; \
	  fi; \
	  echo "migrate-up: applying $$v"; \
	  psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 --single-transaction -q -f "$$f"; \
	  psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -q -c "INSERT INTO schema_migrations (version) VALUES ('$$v')"; \
	done; \
	echo "migrate-up: done"

migrate-down: ## Roll back all *.down.sql migrations, newest first (DESTRUCTIVE)
	@command -v psql >/dev/null || { echo "psql not found"; exit 1; }
	@echo "This DROPS the schema in $(DATABASE_URL). Ctrl-C within 3s to abort."; sleep 3
	@set -euo pipefail; \
	for f in $$(ls -r $(MIGRATIONS)/*.down.sql); do \
	  v=$$(basename "$$f" .down.sql); \
	  echo "migrate-down: reverting $$v"; \
	  psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 --single-transaction -q -f "$$f"; \
	  psql "$(DATABASE_URL)" -q -c "DELETE FROM schema_migrations WHERE version='$$v'" || true; \
	done; \
	echo "migrate-down: done"

# ── Compose ───────────────────────────────────────────────────────────────
up: ## Bring up the full stack (postgres, minio, migrations, 6 services, 2 agents, ui)
	$(COMPOSE) up --build -d
	@echo
	@echo "UI      http://localhost:$${NEXUS_UI_PORT:-3000}"
	@echo "API     http://localhost:$${NEXUS_API_PORT:-8080}/v1/namespaces/$(NAMESPACE)/runs"
	@echo "MinIO   http://localhost:$${NEXUS_MINIO_CONSOLE_PORT:-9001}"
	@echo
	@echo "Next:   make seed   # create a dev API key"

down: ## Stop the stack and remove volumes (DESTROYS local run history)
	$(COMPOSE) down -v

logs: ## Tail logs for all services
	$(COMPOSE) logs -f --tail=100

ps: ## Show container status
	$(COMPOSE) ps

# ── UI ────────────────────────────────────────────────────────────────────
ui-dev: ## Run the Vite dev server against a local gateway
	cd ui && npm install && npm run dev

ui-build: ## Typecheck and production-build the UI
	cd ui && npm install && npm run build

ui-test: ## Run the UI component tests
	cd ui && npm test

# ── Dev data ──────────────────────────────────────────────────────────────
# The key is inserted hashed (api_keys.key_hash), matching how the gateway
# verifies it: a plaintext key in the database would defeat the point of hashing
# at rest even in dev.
seed: ## Create a dev API key (prints the plaintext once)
	@command -v psql >/dev/null || { echo "psql not found; run: $(COMPOSE) exec postgres psql -U nexus -d nexus"; exit 1; }
	@set -euo pipefail; \
	KEY="$${NEXUS_DEV_API_KEY:-dev-admin-key}"; \
	HASH=$$(printf '%s' "$$KEY" | shasum -a 256 | cut -d' ' -f1); \
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -q -c \
	  "INSERT INTO api_keys (namespace, name, key_hash, role) \
	   VALUES ('$(NAMESPACE)', 'dev-admin', '$$HASH', 'admin') \
	   ON CONFLICT (key_hash) DO NOTHING"; \
	WKEY="$${NEXUS_WORKER_API_KEY:-dev-worker-key}"; \
	WHASH=$$(printf '%s' "$$WKEY" | shasum -a 256 | cut -d' ' -f1); \
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -q -c \
	  "INSERT INTO api_keys (namespace, name, key_hash, role) \
	   VALUES ('$(NAMESPACE)', 'dev-worker', '$$WHASH', 'worker') \
	   ON CONFLICT (key_hash) DO NOTHING"; \
	echo "seed: admin  key = $$KEY"; \
	echo "seed: worker key = $$WKEY  (matches the agents' NEXUS_WORKER_API_KEY)"; \
	echo "seed: DEV ONLY — these are not secrets."

# EXAMPLE selects which of examples/*/compiled.yaml to submit. ten_node_mixed is
# the default because it IS the Phase 1 exit criterion: 10 nodes, mixed CPU/GPU.
EXAMPLE ?= ten_node_mixed

submit-example: ## Submit an example DAG and create a run (EXAMPLE=ten_node_mixed)
	@test -f examples/$(EXAMPLE)/compiled.yaml || { echo "examples/$(EXAMPLE)/compiled.yaml not found (examples/ is owned by the CLI/SDK workstream)"; exit 1; }
	$(GO) run ./cli/cmd/nexusctl workflow submit -f examples/$(EXAMPLE)/compiled.yaml --namespace $(NAMESPACE)
	@echo "Now create a run: go run ./cli/cmd/nexusctl run create --spec <name-from-compiled.yaml> --namespace $(NAMESPACE)"

clean: ## Remove build artefacts
	rm -rf ui/dist ui/node_modules/.vite
	$(GO) clean -cache -testcache
