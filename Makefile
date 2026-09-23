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
        ui-dev ui-build ui-test seed submit-example chaos clean \
        backup backup-artifacts backup-all restore-drill artifact-gc check-refs \
        license-report license-check dev-certs up-tls certs-check

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

# The chaos suite drives the REAL compose stack: it submits a run, waits for a
# task to be executing, SIGKILLs that worker's container and asserts the platform
# recovered with exactly one logical effect. It needs the stack up and an admin
# key, takes minutes, and deliberately breaks the fleet -- hence its own build
# tag, so `go test ./...` can never trip over it.
chaos: ## Kill workers mid-task and assert exactly-one-logical-effect (needs `make up` + NEXUS_API_KEY)
	@test -n "$$NEXUS_API_KEY" || { echo "NEXUS_API_KEY is unset; run 'make seed' and export it"; exit 1; }
	$(GO) test -tags chaos -count=1 -v -timeout 20m ./tests/chaos/

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
	@echo "API     http://localhost:$${NEXUS_API_PORT:-8090}/v1/namespaces/$(NAMESPACE)/runs"
	@echo "MinIO   http://localhost:$${NEXUS_MINIO_CONSOLE_PORT:-9001}"
	@echo
	@echo "Next:   make seed   # create a dev API key"

down: ## Stop the stack and remove volumes (DESTROYS local run history)
	$(COMPOSE) down -v

logs: ## Tail logs for all services
	$(COMPOSE) logs -f --tail=100

ps: ## Show container status
	$(COMPOSE) ps

# ── TLS (docs/TLS.md §5) ──────────────────────────────────────────────────
# Opt-in. `make up` stays plaintext; `make up-tls` layers the mTLS override on
# top. The generated material, including a CA private key, lands in .dev-certs/
# which is gitignored -- nothing here is ever committed.
COMPOSE_TLS   ?= $(COMPOSE) -f deploy/docker-compose.tls.yml

dev-certs: ## Generate a local CA + per-service TLS leaves into .dev-certs/ (gitignored)
	NEXUS_CERT_DIR=.dev-certs deploy/nexus-certs.sh dev

up-tls: ## Bring up the stack with mTLS between every gRPC hop (run dev-certs first)
	@test -f .dev-certs/ca-bundle.pem || { echo "no certificates yet; run 'make dev-certs' first"; exit 1; }
	$(COMPOSE_TLS) up --build -d

certs-check: ## Show expiry of every cert in .dev-certs; nonzero if any is within 7 days
	deploy/nexus-certs.sh check .dev-certs

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
seed: ## Generate and install API keys (prints each plaintext exactly once)
	@command -v psql >/dev/null || { echo "psql not found; run: $(COMPOSE) exec postgres psql -U nexus -d nexus"; exit 1; }
	@set -euo pipefail; \
	gen() { \
	  if command -v openssl >/dev/null 2>&1; then \
	    printf 'nxs_%s_%s' "$$1" "$$(openssl rand -hex 32)"; \
	  else \
	    printf 'nxs_%s_%s' "$$1" "$$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"; \
	  fi; \
	}; \
	KEY="$${NEXUS_DEV_API_KEY:-$$(gen admin)}"; \
	HASH=$$(printf '%s' "$$KEY" | shasum -a 256 | cut -d' ' -f1); \
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -q -c \
	  "INSERT INTO api_keys (namespace, name, key_hash, role) \
	   VALUES ('$(NAMESPACE)', 'admin-$$(date +%Y%m%d%H%M%S)', '$$HASH', 'admin') \
	   ON CONFLICT (key_hash) DO NOTHING"; \
	WKEY="$${NEXUS_WORKER_API_KEY:-$$(gen worker)}"; \
	WHASH=$$(printf '%s' "$$WKEY" | shasum -a 256 | cut -d' ' -f1); \
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -q -c \
	  "INSERT INTO api_keys (namespace, name, key_hash, role) \
	   VALUES ('$(NAMESPACE)', 'worker-$$(date +%Y%m%d%H%M%S)', '$$WHASH', 'worker') \
	   ON CONFLICT (key_hash) DO NOTHING"; \
	echo ""; \
	echo "  admin  key (console, CLI):  $$KEY"; \
	echo "  worker key (agents):        $$WKEY"; \
	echo ""; \
	echo "  Only the SHA-256 hash is stored, so these cannot be recovered."; \
	echo "  Copy the admin key into the console via the header's \"Set API key\"."; \
	echo "  Give the worker key to the agents as NEXUS_WORKER_API_KEY, then:"; \
	echo "      NEXUS_WORKER_API_KEY=$$WKEY $(COMPOSE) up -d agent-cpu agent-gpu"; \
	echo ""

# EXAMPLE selects which of examples/*/compiled.yaml to submit. ten_node_mixed is
# the default because it IS the Phase 1 exit criterion: 10 nodes, mixed CPU/GPU.
EXAMPLE ?= ten_node_mixed

submit-example: ## Submit an example DAG and create a run (EXAMPLE=ten_node_mixed)
	@test -f examples/$(EXAMPLE)/compiled.yaml || { echo "examples/$(EXAMPLE)/compiled.yaml not found (examples/ is owned by the CLI/SDK workstream)"; exit 1; }
	$(GO) run ./cli/cmd/nexusctl workflow submit -f examples/$(EXAMPLE)/compiled.yaml --namespace $(NAMESPACE)
	@echo "Now create a run: go run ./cli/cmd/nexusctl run create --spec <name-from-compiled.yaml> --namespace $(NAMESPACE)"

# ── Backup and restore ────────────────────────────────────────────────────
# BACKUP_DIR is passed through rather than baked in, so the same targets work
# against a laptop directory and a mounted backup volume.
BACKUP_DIR ?= ./backups

backup: ## Dump Postgres only (see backup-all: this is HALF a backup)
	BACKUP_DIR=$(BACKUP_DIR) deploy/backup.sh

backup-artifacts: ## Mirror the MinIO result bucket only (the other half)
	BACKUP_DIR=$(BACKUP_DIR) deploy/backup-artifacts.sh

# The target to actually use. Postgres alone leaves large output_refs pointing at
# objects nobody copied, so `make backup` is deliberately NOT the complete one --
# backup-all runs both halves in the order that makes the skew safe (objects
# first, then Postgres; see deploy/backup-artifacts.sh).
backup-all: ## Back up BOTH stores in the correct order (use this one)
	BACKUP_DIR=$(BACKUP_DIR) deploy/backup-all.sh

# Uses scratch buckets and a scratch database; never touches live data.
restore-drill: ## Prove a restore recovers artifacts byte-identically (needs `make up`)
	deploy/restore-drill.sh

# ── Artifact GC (ADR-0008) ────────────────────────────────────────────────
# The restore procedure in OPERATIONS §7.2 deliberately produces orphan objects
# -- objects no database row references -- and until now nothing reclaimed them.
#
# There is NO delete target here, and that is deliberate. `make artifact-gc`
# reports; deleting requires typing the two flags out in full, so a destructive
# run is always an explicit act and never something a tab-completed make target
# can do by accident.
ARTIFACT_GC_ENDPOINT ?= http://localhost:$(or $(NEXUS_MINIO_PORT),9010)
ARTIFACT_GC_DSN      ?= postgres://nexus:nexus-dev-password@localhost:55433/nexus?sslmode=disable

artifact-gc: ## Report orphaned result-store objects; deletes NOTHING (ADR-0008)
	$(GO) run ./services/artifact-gc/cmd/artifact-gc \
	  -database-url '$(ARTIFACT_GC_DSN)' -endpoint '$(ARTIFACT_GC_ENDPOINT)' $(ARTIFACT_GC_FLAGS)

# The OPPOSITE direction: rows naming an object that is GONE. Read-only by
# construction, and exits nonzero when any are found so it can be used as a
# monitoring check. §7.2's backup ordering is designed to produce orphans and
# never dangling refs, so a hit here is a real fault rather than routine drift.
check-refs: ## Report rows whose result object is MISSING; read-only (OPERATIONS 7.6)
	$(GO) run ./services/artifact-gc/cmd/artifact-gc -check-refs \
	  -database-url '$(ARTIFACT_GC_DSN)' -endpoint '$(ARTIFACT_GC_ENDPOINT)' $(ARTIFACT_GC_FLAGS)

clean: ## Remove build artefacts
	rm -rf ui/dist ui/node_modules/.vite
	$(GO) clean -cache -testcache

# Licensing. The inventory in docs/THIRD-PARTY-LICENSES.md is generated, so it
# can be re-checked against go.mod rather than trusted to stay current by hand.
license-report: ## List the license of every module compiled into the binaries
	@GOMODCACHE=$$($(GO) env GOMODCACHE); \
	$(GO) list -deps ./... 2>/dev/null | grep -E '^[a-z0-9.-]+\.[a-z]+/' \
	  | xargs $(GO) list -f '{{if .Module}}{{.Module.Path}} {{.Module.Version}}{{end}}' 2>/dev/null \
	  | sort -u | grep -v khuriwalgroup | while read -r p v; do \
	    [ -z "$$p" ] && continue; \
	    d="$$GOMODCACHE/$$p@$$v"; \
	    lic=$$(ls "$$d" 2>/dev/null | grep -iE '^(LICENSE|COPYING)' | head -1); \
	    t="UNKNOWN"; \
	    if [ -n "$$lic" ]; then \
	      if grep -qi "Apache License" "$$d/$$lic"; then t="Apache-2.0"; \
	      elif grep -qi "Permission is hereby granted, free of charge" "$$d/$$lic"; then t="MIT"; \
	      elif grep -qi "Redistribution and use in source and binary" "$$d/$$lic"; then t="BSD"; fi; \
	    fi; \
	    printf '%-60s %-40s %s\n' "$$p" "$$v" "$$t"; \
	  done

# Fails when a first-party source file is missing its SPDX identifier. Generated
# code is exempt: it is rewritten by protoc and would lose the header anyway.
license-check: ## Assert every first-party source file carries an SPDX identifier
	@missing=0; \
	for f in $$(git ls-files '*.go' '*.ts' '*.tsx' '*.py' '*.sh' '*.sql' '*.proto' \
	            | grep -v '/gen/' | grep -v '^gen/'); do \
	  if ! head -5 "$$f" | grep -q 'SPDX-License-Identifier'; then \
	    echo "missing SPDX header: $$f"; missing=1; \
	  fi; \
	done; \
	if [ $$missing -eq 0 ]; then echo "SPDX headers: OK"; else exit 1; fi
