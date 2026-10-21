# Nexus development Makefile.
#
# `help` is the default target because a Makefile whose default action is a
# surprise build is a Makefile people stop using.

SHELL := /bin/bash
.DEFAULT_GOAL := help

# The development stack runs on the HOST's PostgreSQL (the project rule:
# Postgres comes from the host, never a container), in its own database
# $(NEXUS_DEV_DB), created on first `make up` and dropped by `make down`; the
# compose file's default. NEXUS_BUNDLED_POSTGRES=1 opts in to its postgres
# container instead (deploy/docker-compose.bundled-postgres.yml): for a
# machine with no PostgreSQL and CI runners. The PITR overlay always uses it.
NEXUS_BUNDLED_POSTGRES ?=
export NEXUS_BUNDLED_POSTGRES
NEXUS_DEV_DB  ?= nexus_dev_compose
COMPOSE_BASE  ?= docker compose -f deploy/docker-compose.yml
COMPOSE_BUNDLED ?= $(COMPOSE_BASE) -f deploy/docker-compose.bundled-postgres.yml
ifeq ($(NEXUS_BUNDLED_POSTGRES),1)
COMPOSE       ?= $(COMPOSE_BUNDLED)
else
COMPOSE       ?= $(COMPOSE_BASE)
# What the stack's containers connect to; compose needs it to parse the file
# even for down/logs/ps. Computed once (string work only, no database access).
ifeq ($(origin NEXUS_HOST_DATABASE_URL),undefined)
NEXUS_HOST_DATABASE_URL := $(shell tests/lib/hostpg-db.sh container-url $(NEXUS_DEV_DB))
endif
export NEXUS_HOST_DATABASE_URL
endif
GO            ?= go
PROTOC        ?= protoc
# protoc plugins install into GOBIN, which is not on PATH in every shell.
GOBIN         ?= $(shell $(GO) env GOPATH)/bin
NAMESPACE     ?= default
# The tenant `make seed` mints the worker (agent) key for. Defaults to NAMESPACE;
# set it with NEXUS_AGENT_NAMESPACE when the compose agents serve another tenant.
AGENT_NAMESPACE ?= $(or $(NEXUS_AGENT_NAMESPACE),$(NAMESPACE))

# The database `make up` uses, which is what seed and the migrate targets act
# on unless told otherwise: $(NEXUS_DEV_DB) on the host's server, or the
# bundled container's with NEXUS_BUNDLED_POSTGRES=1. It used to default to a
# bare "local dev database" on 55432 that nothing here creates -- so on a
# machine where something else owned 55432, `make seed` (and the
# schema-dropping `make migrate-down`) aimed at an unrelated database. Override
# DATABASE_URL to target another one.
NEXUS_POSTGRES_PORT ?= 55433
ifeq ($(NEXUS_BUNDLED_POSTGRES),1)
DATABASE_URL  ?= postgres://nexus:nexus-dev-password@localhost:$(NEXUS_POSTGRES_PORT)/nexus?sslmode=disable
else
DATABASE_URL  ?= $(shell tests/lib/hostpg-db.sh url $(NEXUS_DEV_DB))
endif
# The PostgreSQL server the test and bench targets create their throwaway
# databases on: the workstation's own server by default (the project rule:
# Postgres comes from the host, never a container; tests/lib/hostpg.sh). An
# admin DSN for a role that may CREATE DATABASE.
NEXUS_TEST_PG_SERVER ?= postgres://127.0.0.1:5432/postgres?sslmode=disable
export NEXUS_TEST_PG_SERVER
# The gateway as `make up` publishes it (deploy/docker-compose.yml maps
# NEXUS_API_PORT on the host to the gateway's 8080 in the container).
NEXUS_API_PORT ?= 8090
NEXUS_ENDPOINT ?= http://localhost:$(NEXUS_API_PORT)
MIGRATIONS    := services/migrations

# test-nats and sdk-test have their own .PHONY lines so a merge that edits the
# list below does not conflict with them.
.PHONY: test-nats test-vault airgap-bundle airgap-test
.PHONY: sdk-test
.PHONY: sdk-typecheck
.PHONY: check-doc-refs
.PHONY: check-go-version
.PHONY: objstore-conformance tracing-e2e k8s-taskapi-e2e test-stores-up test-stores-down
.PHONY: docs docs-serve docs-clean
.PHONY: help proto build test lint migrate-up migrate-down up down logs ps \
        ui-dev ui-build ui-test e2e e2e-up e2e-down seed submit-example chaos clean \
        backup backup-artifacts backup-all restore-drill artifact-gc check-refs \
        up-pitr backup-base pitr-verify restore-pitr pitr-drill \
        license-report license-check dev-certs up-tls certs-check \
        k8s-dev-images k8s-dev-up k8s-dev-test k8s-dev-test-long k8s-dev-status k8s-dev-down bench-pg bench-pg-down bench-dispatch bench-inline

help: ## Show this help
	@echo "Nexus — make targets"
	@echo
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
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

test: ## Run Go, UI and Python SDK unit tests
	$(GO) test ./...
	$(MAKE) ui-test
	$(MAKE) sdk-test

lint: ## go vet over the whole module
	$(GO) vet ./...

# The chaos suite drives the REAL compose stack: it submits a run, waits for a
# task to be executing, SIGKILLs that worker's container and asserts the platform
# recovered with exactly one logical effect. It needs the stack up and an admin
# key, takes minutes, and deliberately breaks the fleet -- hence its own build
# tag, so `go test ./...` can never trip over it.
#
# NEXUS_COMPOSE_FILE takes the stack's files, comma-separated, in the order it
# was brought up with (overlays included); NEXUS_CHAOS_DATABASE_URL is the
# stack's database as the host reaches it, for a stack on the host's
# PostgreSQL. The example sweep gives each example its own budget
# (tests/chaos/config.go; NEXUS_CHAOS_EXAMPLE_BUDGET, _SCALE, NEXUS_CHAOS_EXAMPLES),
# so the whole suite can take well over the 20 minutes it once had.
CHAOS_TIMEOUT ?= 180m
chaos: ## Kill workers mid-task and assert exactly-one-logical-effect (needs `make up` + NEXUS_API_KEY)
	@test -n "$$NEXUS_API_KEY" || { echo "NEXUS_API_KEY is unset; run 'make seed' and export it"; exit 1; }
	$(GO) test -tags chaos -count=1 -v -timeout $(CHAOS_TIMEOUT) ./tests/chaos/

# The outbox relay and dispatch hints against a real NATS JetStream (throwaway
# brokers nexus-nats-agent-*, host ports 55512-55514, so the broker-outage test
# cannot disturb anything else) and a throwaway database on the host's
# PostgreSQL (NEXUS_TEST_PG_SERVER; never a Postgres container), dropped
# afterwards. Also runs the broker AUTHORIZATION suite against the compose- and
# Helm-rendered configurations (needs helm). KEEP=1 leaves both for inspection.
test-nats: ## Run the NATS/outbox and broker-authorization integration tests (host PostgreSQL, throwaway brokers; needs Docker, helm, psql)
	tests/nats/run-integration.sh

# Worker-side secret resolution against a REAL Vault (ADR-0025, OPERATIONS
# §25.1). A throwaway dev-mode container, pinned by digest, on its own port;
# the test seals it at the end (NEXUS_TEST_VAULT_ALLOW_SEAL), which is why the
# container is always removed. KEEP=1 leaves it up.
VAULT_IMAGE      ?= docker.io/hashicorp/vault:1.21.4@sha256:4e33b126a59c0c333b76fb4e894722462659a6bec7c48c9ee8cea56fccfd2569
VAULT_TEST_NAME  ?= nexus-test-vault
VAULT_TEST_PORT  ?= 55561
test-vault: ## Run the secret-provider integration tests against a pinned dev-mode Vault container (needs Docker)
	@docker rm -f $(VAULT_TEST_NAME) >/dev/null 2>&1 || true
	docker run -d --name $(VAULT_TEST_NAME) --cap-add IPC_LOCK -p 127.0.0.1:$(VAULT_TEST_PORT):8200 \
	  -e VAULT_DEV_ROOT_TOKEN_ID=nexus-test-root -e VAULT_DEV_LISTEN_ADDRESS=0.0.0.0:8200 '$(VAULT_IMAGE)' >/dev/null
	@for i in $$(seq 1 30); do curl -fsS http://127.0.0.1:$(VAULT_TEST_PORT)/v1/sys/health >/dev/null 2>&1 && break; sleep 1; done
	NEXUS_TEST_VAULT_ADDR=http://127.0.0.1:$(VAULT_TEST_PORT) NEXUS_TEST_VAULT_ROOT_TOKEN=nexus-test-root \
	  NEXUS_TEST_VAULT_ALLOW_SEAL=1 $(GO) test -count=1 -run 'Vault' -v ./agent/internal/secrets/; \
	  rc=$$?; [ "$(KEEP)" = "1" ] || docker rm -f $(VAULT_TEST_NAME) >/dev/null; exit $$rc

# ── Benchmarks (ADR-0016, ADR-0018) ───────────────────────────────────────
# The benchmarks write hundreds of thousands of rows, so they never default to a
# database: BENCH_DSN must be set explicitly, and `make bench-pg` provides a
# disposable, migrated database $(BENCH_DB) on the host's PostgreSQL
# (NEXUS_TEST_PG_SERVER; never a Postgres container) plus a MinIO container
# for bench-inline; `make bench-pg-down` drops and removes them. Numbers from a
# laptop locate the knee of the curve; they are not production capacity
# figures. The dispatch benchmark at 16 replicas opens far more connections
# than a server shared with other workloads has free: point
# NEXUS_TEST_PG_SERVER at a dedicated server for published figures, and record
# its settings with them.
BENCH_NAME        ?= nexus-bench
BENCH_DB          ?= nexus_bench
BENCH_MINIO_PORT  ?= 55542
BENCH_DSN         ?=
BENCH_MINIO       ?= localhost:$(BENCH_MINIO_PORT)
# The S3 store for benchmarks and disposable test stores. Silo, the community
# MinIO fork, pinned by digest: MinIO no longer publishes pullable images (F111).
# Keep in step with deploy/docker-compose.yml.
S3_IMAGE          ?= docker.io/pgsty/silo:RELEASE.2026-09-16T00-00-00Z@sha256:635197cb9f36d01bee221d34d1c7d7960f6a95c48b0b6c01d99cd13bdae51a46

# The Azure Blob and GCS emulators for the result-store conformance suite
# (ADR-0027), and the OpenTelemetry backend for the tracing end-to-end test
# (ADR-0026). Pinned by digest like the S3 image; bump deliberately.
AZURITE_IMAGE     ?= mcr.microsoft.com/azure-storage/azurite:3.35.0@sha256:647c63a91102a9d8e8000aab803436e1fc85fbb285e7ce830a82ee5d6661cf37
FAKE_GCS_IMAGE    ?= docker.io/fsouza/fake-gcs-server:1.52.3@sha256:666f86b873120818b10a5e68d99401422fcf8b00c1f27fe89599c35236f48b4c
JAEGER_IMAGE      ?= docker.io/jaegertracing/jaeger:2.10.0@sha256:8c593d8048d86334eb52dba26633b6c846b8ecbe5c22905702c932ae97baeb42
# Disposable test stores: container prefix and loopback ports, overridable so
# parallel checkouts do not collide. PostgreSQL is not one of them: the suites
# that need it use the host's server (NEXUS_TEST_PG_SERVER).
TEST_STORES_NAME  ?= nexus-teststores
TEST_S3_PORT      ?= 55642
TEST_AZURITE_PORT ?= 55643
TEST_GCS_PORT     ?= 55644
TEST_OTLP_PORT    ?= 55645
TEST_JAEGER_PORT  ?= 55646
TEST_S3_USER      ?= teststores
TEST_S3_SECRET    ?= teststores-secret
# Azurite's published development account (not a secret; documented by Microsoft).
AZURITE_KEY       := Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==
TEST_STORE_ENV = \
  NEXUS_TEST_S3_ENDPOINT=127.0.0.1:$(TEST_S3_PORT) NEXUS_TEST_S3_ACCESS_KEY_ID=$(TEST_S3_USER) \
  NEXUS_TEST_S3_SECRET_ACCESS_KEY=$(TEST_S3_SECRET) \
  NEXUS_TEST_AZBLOB_ENDPOINT=http://127.0.0.1:$(TEST_AZURITE_PORT)/devstoreaccount1 \
  NEXUS_TEST_AZBLOB_ACCOUNT=devstoreaccount1 NEXUS_TEST_AZBLOB_KEY='$(AZURITE_KEY)' \
  NEXUS_TEST_GCS_ENDPOINT=http://127.0.0.1:$(TEST_GCS_PORT)

test-stores-up: ## Start pinned Silo, Azurite, fake-gcs-server and Jaeger for the storage and tracing suites
	@docker inspect $(TEST_STORES_NAME)-s3 >/dev/null 2>&1 || docker run -d --name $(TEST_STORES_NAME)-s3 \
	  -e MINIO_ROOT_USER=$(TEST_S3_USER) -e MINIO_ROOT_PASSWORD=$(TEST_S3_SECRET) \
	  -p 127.0.0.1:$(TEST_S3_PORT):9000 $(S3_IMAGE) server /data >/dev/null
	@docker inspect $(TEST_STORES_NAME)-azurite >/dev/null 2>&1 || docker run -d --name $(TEST_STORES_NAME)-azurite \
	  -p 127.0.0.1:$(TEST_AZURITE_PORT):10000 $(AZURITE_IMAGE) \
	  azurite-blob --blobHost 0.0.0.0 --blobPort 10000 --skipApiVersionCheck --loose >/dev/null
	@# -external-url makes the resumable-upload Location point back at the published port.
	@docker inspect $(TEST_STORES_NAME)-gcs >/dev/null 2>&1 || docker run -d --name $(TEST_STORES_NAME)-gcs \
	  -p 127.0.0.1:$(TEST_GCS_PORT):4443 $(FAKE_GCS_IMAGE) -scheme http -port 4443 \
	  -public-host 127.0.0.1:$(TEST_GCS_PORT) -external-url http://127.0.0.1:$(TEST_GCS_PORT) -backend memory >/dev/null
	@docker inspect $(TEST_STORES_NAME)-jaeger >/dev/null 2>&1 || docker run -d --name $(TEST_STORES_NAME)-jaeger \
	  -p 127.0.0.1:$(TEST_OTLP_PORT):4317 -p 127.0.0.1:$(TEST_JAEGER_PORT):16686 $(JAEGER_IMAGE) >/dev/null
	@sleep 2

test-stores-down: ## Remove the containers test-stores-up started
	-docker rm -f $(TEST_STORES_NAME)-s3 $(TEST_STORES_NAME)-azurite \
	  $(TEST_STORES_NAME)-gcs $(TEST_STORES_NAME)-jaeger

objstore-conformance: test-stores-up ## Run the result-store conformance suite against Silo, Azurite and fake-gcs-server
	@# A skipped backend looks like a pass in `go test` output (F85), so a skip fails the target.
	@out=$$($(TEST_STORE_ENV) $(GO) test -count=1 -v -run Conformance ./internal/objstore/... 2>&1); status=$$?; \
	  echo "$$out" | grep -E '^(--- |ok|FAIL|PASS)|skipping'; \
	  if echo "$$out" | grep -q "skipping integration test"; then echo "a backend was SKIPPED"; exit 1; fi; exit $$status

# The test creates (and drops) its own database on the host's PostgreSQL.
tracing-e2e: test-stores-up ## One trace from API to agent and back, read from Jaeger (ADR-0026)
	$(TEST_STORE_ENV) NEXUS_E2E_PG_DSN='$(NEXUS_TEST_PG_SERVER)' \
	  NEXUS_E2E_OTLP_GRPC=127.0.0.1:$(TEST_OTLP_PORT) NEXUS_E2E_JAEGER_QUERY=http://127.0.0.1:$(TEST_JAEGER_PORT) \
	  $(GO) test -tags tracing_e2e -count=1 -v -timeout 10m ./tests/tracing/

# F148 end to end on a LOCAL cluster (k8stest's guard): a kubernetes task pod
# reports usage, takes shared permits, checkpoints and resumes through the
# agent's workload-API listener, then the transport is attacked from outside.
# Needs a MIGRATED database the suite may write (NEXUS_TEST_DATABASE_URL); it
# never creates or drops one. K8S_E2E_NAMESPACE is created and deleted.
K8S_E2E_NAMESPACE ?= nexus-k8s-e2e
k8s-taskapi-e2e: test-stores-up ## Kubernetes task pods through the workload API, into the ledger (F148; needs NEXUS_TEST_DATABASE_URL, docker-desktop/kind)
	@test -n "$$NEXUS_TEST_DATABASE_URL" || { echo "set NEXUS_TEST_DATABASE_URL to a migrated database"; exit 1; }
	$(TEST_STORE_ENV) NEXUS_K8S_CONFORMANCE=1 NEXUS_K8S_TEST_NAMESPACE=$(K8S_E2E_NAMESPACE) \
	  $(GO) test -tags k8s_e2e -count=1 -v -timeout 20m ./agent/e2e/k8staskapi/

print-s3-image: ## Print the pinned S3 (Silo) image reference, for CI and scripts
	@echo '$(S3_IMAGE)'

verify-s3-image: ## Verify the pinned S3 image's GitHub build provenance (needs gh, network)
	@command -v gh >/dev/null || { echo "gh not found; install the GitHub CLI"; exit 1; }
	gh attestation verify 'oci://$(S3_IMAGE)' --repo pgsty/silo \
	  --signer-workflow pgsty/silo/.github/workflows/docker-release.yml

check-doc-refs: ## Fail if any "OPERATIONS §N" reference in the repo names a section that does not exist
	python3 deploy/scripts/check-operations-refs.py

check-go-version: ## Fail unless go.mod, CONTRACT.md, CI and the images name the same Go (and CI and the console image the same Node)
	$(GO) test -count=1 ./tests/repo

# ── Documentation site (docs-site/README.md) ───────────────────────────────
# MkDocs Material from a hash-locked lock file, in its own venv. The CLI
# reference is generated from a nexusctl built here, the API reference from
# api/openapi.yaml, the SDK reference from docstrings (static analysis). The
# build is --strict (every warning, including a broken link or anchor, fails)
# and is followed by a link/anchor check over the rendered HTML.
DOCS_PYTHON   ?= python3
DOCS_VENV     ?= docs-site/.venv
DOCS_NEXUSCTL ?= docs-site/.venv/bin/nexusctl

$(DOCS_VENV)/.installed: docs-site/requirements.txt
	@$(DOCS_PYTHON) -c 'import sys; sys.exit(sys.version_info < (3, 11))' \
	  || { echo "docs: $(DOCS_PYTHON) is $$($(DOCS_PYTHON) --version 2>&1); the docs build needs 3.11+ (set DOCS_PYTHON)"; exit 1; }
	$(DOCS_PYTHON) -m venv $(DOCS_VENV)
	$(DOCS_VENV)/bin/python -m pip install --quiet --upgrade pip
	$(DOCS_VENV)/bin/python -m pip install --quiet --require-hashes -r docs-site/requirements.txt
	@touch $@

docs: $(DOCS_VENV)/.installed ## Build the documentation site (strict) and check every link and anchor
	$(GO) build -o $(DOCS_NEXUSCTL) ./cli/cmd/nexusctl
	cd docs-site && NO_MKDOCS_2_WARNING=1 NEXUS_DOCS_NEXUSCTL='$(abspath $(DOCS_NEXUSCTL))' \
	  '$(abspath $(DOCS_VENV))/bin/mkdocs' build --strict --clean
	$(DOCS_VENV)/bin/python docs-site/scripts/check_links.py docs-site/site
	@echo "docs: built docs-site/site (open docs-site/site/index.html, or run make docs-serve)"

docs-serve: $(DOCS_VENV)/.installed ## Preview the documentation site at http://127.0.0.1:8000
	$(GO) build -o $(DOCS_NEXUSCTL) ./cli/cmd/nexusctl
	cd docs-site && NO_MKDOCS_2_WARNING=1 NEXUS_DOCS_NEXUSCTL='$(abspath $(DOCS_NEXUSCTL))' \
	  '$(abspath $(DOCS_VENV))/bin/mkdocs' serve --strict

docs-clean: ## Remove the built site and the docs venv
	rm -rf docs-site/site $(DOCS_VENV)

bench-pg: ## Create a disposable, migrated database on the host's PostgreSQL + a MinIO container for the benchmarks
	@docker inspect $(BENCH_NAME)-minio >/dev/null 2>&1 || docker run -d --name $(BENCH_NAME)-minio \
	  -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin -p 127.0.0.1:$(BENCH_MINIO_PORT):9000 \
	  $(S3_IMAGE) server /data >/dev/null
	@dsn=$$(NEXUS_TEST_PG_SERVER='$(NEXUS_TEST_PG_SERVER)' HOSTPG_TAG=org.kgf-nexus.bench tests/lib/hostpg-db.sh create $(BENCH_DB)) && \
	  echo "export BENCH_DSN='$$dsn'"

bench-pg-down: ## Drop the benchmark database and remove the MinIO container
	-NEXUS_TEST_PG_SERVER='$(NEXUS_TEST_PG_SERVER)' HOSTPG_TAG=org.kgf-nexus.bench tests/lib/hostpg-db.sh drop $(BENCH_DB)
	-docker rm -f $(BENCH_NAME)-minio

bench-dispatch: ## Claims/sec and p50/p99 claim latency vs replicas and backlog (needs BENCH_DSN)
	@test -n "$(BENCH_DSN)" || { echo "BENCH_DSN is unset; run 'make bench-pg' and export what it prints"; exit 1; }
	NEXUS_BENCH_DSN='$(BENCH_DSN)' $(GO) test -tags bench -count=1 -v -timeout 120m \
	  -run TestDispatchCeiling ./tests/bench/

bench-ratelimit: ## Permit throughput/latency, saturation correctness, and claims with permit traffic (ADR-0032; needs BENCH_DSN)
	@test -n "$(BENCH_DSN)" || { echo "BENCH_DSN is unset; run 'make bench-pg' and export what it prints"; exit 1; }
	NEXUS_BENCH_DSN='$(BENCH_DSN)' $(GO) test -tags bench -count=1 -v -timeout 60m \
	  -run TestRateLimiterCeiling ./tests/bench/

bench-inline: ## Postgres-inline vs MinIO round trip by payload size (needs BENCH_DSN)
	@test -n "$(BENCH_DSN)" || { echo "BENCH_DSN is unset; run 'make bench-pg' and export what it prints"; exit 1; }
	NEXUS_BENCH_DSN='$(BENCH_DSN)' NEXUS_BENCH_MINIO='$(BENCH_MINIO)' $(GO) test -tags bench -count=1 -v \
	  -timeout 30m -run TestInlineVersusBlob ./tests/bench/

# ── Migrations ────────────────────────────────────────────────────────────
# Applied by a job, never by a service at startup (ADR-0011 rule 4). These
# targets are the local equivalent of that job.
migrate-up: ## Apply all *.up.sql migrations to DATABASE_URL
	@command -v psql >/dev/null || { echo "psql not found; use 'make up' to run the compose migration job instead"; exit 1; }
	@# The SAME script the compose/Helm migration job runs, so the local target
	@# cannot drift from it. The inline loop this replaced ignored the
	@# `-- migrate: no-transaction` opt-out and failed on 0015 (CREATE INDEX
	@# CONCURRENTLY inside --single-transaction), which broke every CI
	@# integration job that runs `make migrate-up`.
	DATABASE_URL='$(DATABASE_URL)' MIGRATIONS_DIR=$(MIGRATIONS) sh deploy/migrate.sh

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
# deploy/docker-compose.yml requires NEXUS_WORKER_API_KEY (the agents'
# credential), and Compose refuses to parse the file while it is unset. On a
# fresh checkout nothing has set it, so `make up` used to die inside Compose
# with a bare interpolation error. The first-run entry point is `make
# quickstart` (nexusctl up), which mints both credentials through the API;
# `make up` is the drive-it-yourself path, and now says so before Compose runs.
up: ## Bring up the full stack with your own keys (first run on a fresh checkout: `make quickstart`)
	@if [ -z "$${NEXUS_WORKER_API_KEY:-}" ]; then \
	  echo "make up: NEXUS_WORKER_API_KEY is not set, and the agents cannot start without it." >&2; \
	  echo "" >&2; \
	  echo "  First run on this checkout? Use the quick start: it brings the stack up," >&2; \
	  echo "  registers a tenant and mints the admin and worker tokens through the API" >&2; \
	  echo "  (cli/README.md, 'Local quick start'):" >&2; \
	  echo "      make quickstart" >&2; \
	  echo "" >&2; \
	  echo "  Or drive Compose yourself with a key you generate, then install it:" >&2; \
	  echo '      export NEXUS_WORKER_API_KEY=$$(make -s worker-key)' >&2; \
	  echo "      make up && make seed" >&2; \
	  exit 2; \
	fi
	@if [ "$(NEXUS_BUNDLED_POSTGRES)" != 1 ]; then \
	  HOSTPG_TAG=org.kgf-nexus.dev-compose tests/lib/hostpg-db.sh ensure $(NEXUS_DEV_DB) >/dev/null && \
	  echo "make up: database $(NEXUS_DEV_DB) on the host's PostgreSQL (NEXUS_BUNDLED_POSTGRES=1 for the bundled container)"; \
	fi
	@deploy/scripts/agent-workdir.sh >/dev/null
	$(COMPOSE) up --build -d
	@echo
	@echo "UI      http://localhost:$${NEXUS_UI_PORT:-3000}"
	@echo "API     http://localhost:$${NEXUS_API_PORT:-8090}/v1/namespaces/$(NAMESPACE)/runs"
	@echo "MinIO   http://localhost:$${NEXUS_MINIO_CONSOLE_PORT:-9001}"
	@echo
	@echo "Next:   make seed   # installs this worker key for the agents, and an admin key"

# The first-run path. QUICKSTART_ARGS passes flags through, e.g.
#   make quickstart QUICKSTART_ARGS="-p nexus-mine --api-port 18090"
QUICKSTART_ARGS ?=
.PHONY: quickstart worker-key
quickstart: ## First run: start the stack, register a tenant, mint admin + worker tokens via the API (nexusctl up)
	$(GO) run ./cli/cmd/nexusctl up $(QUICKSTART_ARGS)

worker-key: ## Print a fresh random worker key (export as NEXUS_WORKER_API_KEY before `make up`, then `make seed`)
	@if command -v openssl >/dev/null 2>&1; then printf 'nxs_worker_%s\n' "$$(openssl rand -hex 32)"; \
	else printf 'nxs_worker_%s\n' "$$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"; fi

# These only address existing containers, but Compose still parses the whole
# file, so they pass a placeholder when the key is unset. It reaches no
# container: nothing is created by down, logs or ps.
down: ## Stop the stack and remove volumes and its host database (DESTROYS local run history)
	NEXUS_WORKER_API_KEY="$${NEXUS_WORKER_API_KEY:-unused-by-down}" $(COMPOSE) down -v
	@if [ "$(NEXUS_BUNDLED_POSTGRES)" != 1 ]; then \
	  HOSTPG_TAG=org.kgf-nexus.dev-compose tests/lib/hostpg-db.sh drop $(NEXUS_DEV_DB); \
	fi

logs: ## Tail logs for all services
	@NEXUS_WORKER_API_KEY="$${NEXUS_WORKER_API_KEY:-unused-by-logs}" $(COMPOSE) logs -f --tail=100

ps: ## Show container status
	@NEXUS_WORKER_API_KEY="$${NEXUS_WORKER_API_KEY:-unused-by-ps}" $(COMPOSE) ps

# ── Kubernetes dev environment (deploy/terraform/environments/dev) ────────
# The whole platform on Docker Desktop's Kubernetes, via the kgf-nexus Helm
# chart and Terraform. Runs alongside the compose stack (different ports).
TF_DEV := deploy/terraform/environments/dev
# Local, git-ignored settings for the dev environment (its .gitignore drops
# *.tfvars), such as var.kgf or the KGF.Nexus workspace namespaces to run
# agents for (workspace_worker_namespaces).
TF_DEV_VARFILE := $(TF_DEV)/local.tfvars
TF_DEV_VARS = $(if $(wildcard $(TF_DEV_VARFILE)),-var-file=local.tfvars)

k8s-dev-images: ## Build every image and load it into the local kind node (prints the tag)
	@deploy/scripts/k8s-dev-images.sh

k8s-dev-up: ## Build + load images, then terraform apply the dev environment on Docker Desktop (uses the host Postgres)
	@set -euo pipefail; \
	NEXUS_NATS_DIR=.dev-nats deploy/nats/nexus-nats-auth.sh init --tenant default; \
	for ns in $$(grep -oE 'kgf-[0-9a-f]{32}' '$(TF_DEV_VARFILE)' 2>/dev/null | sort -u); do \
	  NEXUS_NATS_DIR=.dev-nats deploy/nats/nexus-nats-auth.sh add-tenant "$$ns"; \
	done; \
	pgpass=$$(deploy/scripts/dev-host-postgres.sh); \
	export TF_VAR_postgres_password="$$(cat "$$pgpass")"; \
	tag=$$(deploy/scripts/k8s-dev-images.sh | tail -n 1); \
	echo "image tag: $$tag"; \
	terraform -chdir=$(TF_DEV) init -input=false -upgrade=false >/dev/null; \
	terraform -chdir=$(TF_DEV) apply -input=false -auto-approve $(TF_DEV_VARS) -var "image_tag=$$tag"; \
	echo; echo "Admin API key: terraform -chdir=$(TF_DEV) output -raw admin_api_key"

k8s-dev-test: ## Run the workload suite (CSV ETL, ML training, GPU scheduling, fan-out) on the dev environment
	examples/run-suite.sh

k8s-dev-test-long: ## Run the long workloads: credit-risk ML (~11 min) and GPU image processing (~7 min)
	SUITE=long examples/run-suite.sh

k8s-dev-status: ## Show the dev environment's pods and URLs
	kubectl -n nexus-dev get pods,svc
	@terraform -chdir=$(TF_DEV) output

k8s-dev-down: ## Destroy the dev environment (DELETES its data volumes)
	TF_VAR_postgres_password="$$(cat .dev-postgres/password 2>/dev/null || echo unused)" terraform -chdir=$(TF_DEV) destroy -input=false $(TF_DEV_VARS)

# ── Terraform: cloud platforms (deploy/terraform) ─────────────────────────
# OFFLINE only: init -backend=false, validate, and `terraform test` plans with
# mocked providers. No state, no credentials, nothing is ever applied. Needs
# terraform >= 1.11 or Docker, plus network access to download providers.
.PHONY: tf-validate tf-lint
tf-validate: ## Validate every Terraform module/environment offline (fmt, validate, mocked-plan tests)
	deploy/terraform/validate.sh

tf-lint: ## tflint, checkov and trivy over deploy/terraform, in pinned containers (needs Docker)
	deploy/terraform/validate.sh lint

# ── TLS (docs/TLS.md §5) ──────────────────────────────────────────────────
# Opt-in. `make up` stays plaintext; `make up-tls` layers the mTLS override on
# top. The generated material, including a CA private key, lands in .dev-certs/
# which is gitignored -- nothing here is ever committed.
COMPOSE_TLS   ?= $(COMPOSE) -f deploy/docker-compose.tls.yml

dev-certs: ## Generate a local CA + per-service TLS leaves into .dev-certs/ (gitignored)
	NEXUS_CERT_DIR=.dev-certs deploy/nexus-certs.sh dev

up-tls: ## Bring up the stack with mTLS between every gRPC hop (run dev-certs first)
	@test -f .dev-certs/ca-bundle.pem || { echo "no certificates yet; run 'make dev-certs' first"; exit 1; }
	@deploy/scripts/agent-workdir.sh >/dev/null
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

# ── Browser end-to-end (ui/e2e, Playwright) ─────────────────────────────
# An ISOLATED compose stack (its own project name, volumes and host ports; see
# deploy/e2e/), seeded with fresh keys, then the Playwright suite against it.
# Torn down afterwards, pass or fail, unless E2E_KEEP=1. Never touches the
# `make up` stack.
#
#   make e2e                                  # up, test, down
#   make e2e E2E_KEEP=1                       # leave the stack up to debug
#   make e2e E2E_MINIO_IMAGE=<local image>    # optional: another local S3 image
#                                             # instead of the pinned Silo digest
E2E_PROJECT     ?= nexus-e2e
E2E_PORT_BASE   ?= 55711
E2E_MINIO_IMAGE ?=
E2E_KEEP        ?=
E2E_ENV = E2E_PROJECT='$(E2E_PROJECT)' E2E_PORT_BASE='$(E2E_PORT_BASE)' E2E_MINIO_IMAGE='$(E2E_MINIO_IMAGE)'

# The cleanup trap uses absolute paths: the recipe cd's into ui/, and the trap
# runs from there. Teardown failure is reported, never silently ignored.
e2e: ## Browser e2e: isolated compose stack + Playwright, torn down after (E2E_KEEP=1 keeps it)
	@set -euo pipefail; \
	cleanup() { \
	  status=$$?; \
	  if [ $$status -ne 0 ]; then \
	    mkdir -p '$(CURDIR)/ui/e2e/test-results'; \
	    $(E2E_ENV) '$(CURDIR)/deploy/e2e/e2e-stack.sh' logs > '$(CURDIR)/ui/e2e/test-results/stack.log' 2>&1 || true; \
	    echo "e2e: failed; service logs in ui/e2e/test-results/stack.log, traces in ui/e2e/test-results/"; \
	  fi; \
	  if [ '$(E2E_KEEP)' = 1 ]; then echo "e2e: stack $(E2E_PROJECT) left running (make e2e-down)"; \
	  else $(E2E_ENV) '$(CURDIR)/deploy/e2e/e2e-stack.sh' down >/dev/null 2>&1 || echo "e2e: WARNING: teardown of $(E2E_PROJECT) failed; run make e2e-down" >&2; fi; \
	  exit $$status; \
	}; \
	trap cleanup EXIT; \
	$(E2E_ENV) deploy/e2e/e2e-stack.sh up; \
	cd ui; \
	test -d node_modules || npm ci --no-audit --no-fund; \
	npx playwright install chromium; \
	npx playwright test

e2e-up: ## Start (or refresh) the isolated e2e stack and write ui/e2e/.stack.env
	$(E2E_ENV) deploy/e2e/e2e-stack.sh up

e2e-down: ## Destroy the isolated e2e stack and its volumes
	$(E2E_ENV) deploy/e2e/e2e-stack.sh down

# ── Python SDK ────────────────────────────────────────────────────────────
# The SDK's pytest suite, in its own virtualenv so it never installs into (or
# depends on) whatever Python the shell happens to have. .venv-sdk/ is already
# gitignored. The venv is rebuilt only when pyproject.toml changes; delete it to
# force a clean install. SDK_PYTHON must be 3.11+ (pyproject requires-python).
#
# SDK_REPORT_DIR, when set, also writes JUnit and Cobertura XML there. Coverage
# data goes inside the venv so the source tree stays clean.
SDK_DIR        := sdk/python
SDK_PYTHON     ?= python3
SDK_VENV       ?= .venv-sdk
SDK_REPORT_DIR ?=

$(SDK_VENV)/.installed: $(SDK_DIR)/pyproject.toml
	@$(SDK_PYTHON) -c 'import sys; sys.exit(0 if sys.version_info >= (3, 11) else 1)' \
	  || { echo "sdk-test: $(SDK_PYTHON) is $$($(SDK_PYTHON) --version 2>&1); the SDK needs 3.11+ (set SDK_PYTHON)"; exit 1; }
	$(SDK_PYTHON) -m venv $(SDK_VENV)
	$(SDK_VENV)/bin/python -m pip install --quiet --upgrade pip
	$(SDK_VENV)/bin/python -m pip install --quiet -e '$(SDK_DIR)[dev]'
	@touch $@

# mypy --strict over the package and typecheck/typed_usage.py, whose assert_type
# lines pin that a task's return type flows through tracing, .map and
# with_overrides. sdk-test runs it first, so every CI job that runs sdk-test
# type-checks too.
sdk-typecheck: $(SDK_VENV)/.installed ## Type-check the Python SDK (mypy --strict, package + typed usage example)
	cd $(SDK_DIR) && '$(abspath $(SDK_VENV))/bin/python' -m mypy --strict

sdk-test: $(SDK_VENV)/.installed sdk-typecheck ## Type-check, then run the Python SDK tests with coverage (SDK_REPORT_DIR=<dir> adds JUnit/Cobertura XML)
	$(if $(SDK_REPORT_DIR),@mkdir -p '$(SDK_REPORT_DIR)')
	cd $(SDK_DIR) && COVERAGE_FILE='$(abspath $(SDK_VENV))/.coverage' '$(abspath $(SDK_VENV))/bin/python' -m pytest \
	  -p no:cacheprovider --cov=nexus --cov-report=term \
	  $(if $(SDK_REPORT_DIR),--junitxml='$(abspath $(SDK_REPORT_DIR))/sdk-junit.xml' --cov-report=xml:'$(abspath $(SDK_REPORT_DIR))/sdk-coverage.xml')

# ── Dev data ──────────────────────────────────────────────────────────────
# The key is inserted hashed (api_keys.key_hash), matching how the gateway
# verifies it: a plaintext key in the database would defeat the point of hashing
# at rest even in dev.
#
# The namespace's tenant row comes first. Since migration 0014 every api_keys
# row must name a namespace that belongs to a registered tenant, so on a fresh
# database (nothing registered yet) the key inserts failed with "Namespace must
# belong to a registered tenant" (F121). Idempotent, and the same statement the
# Helm bootstrap job and deploy/pitr-seed.sh already run.
seed: ## Generate and install API keys (prints each plaintext exactly once)
	@set -euo pipefail; \
	if ! command -v psql >/dev/null 2>&1; then \
	  if [ "$(NEXUS_BUNDLED_POSTGRES)" != 1 ]; then \
	    echo "seed: no psql on PATH; install the PostgreSQL client (the stack uses the host's server)" >&2; exit 1; \
	  fi; \
	  echo "seed: no psql on PATH; using the compose stack's own postgres container" >&2; \
	  psql() { shift; NEXUS_WORKER_API_KEY="$${NEXUS_WORKER_API_KEY:-unused}" $(COMPOSE) exec -T postgres psql -U nexus -d nexus "$$@"; }; \
	fi; \
	gen() { \
	  if command -v openssl >/dev/null 2>&1; then \
	    printf 'nxs_%s_%s' "$$1" "$$(openssl rand -hex 32)"; \
	  else \
	    printf 'nxs_%s_%s' "$$1" "$$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"; \
	  fi; \
	}; \
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -q -c \
	  "INSERT INTO tenants (namespace, name) VALUES ('$(NAMESPACE)', '$(NAMESPACE)') \
	   ON CONFLICT (namespace) DO NOTHING"; \
	KEY="$${NEXUS_DEV_API_KEY:-$$(gen admin)}"; \
	HASH=$$(printf '%s' "$$KEY" | shasum -a 256 | cut -d' ' -f1); \
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -q -c \
	  "INSERT INTO api_keys (namespace, name, key_hash, role) \
	   VALUES ('$(NAMESPACE)', 'admin-$$(date +%Y%m%d%H%M%S)', '$$HASH', 'admin') \
	   ON CONFLICT (key_hash) DO NOTHING"; \
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -q -c \
	  "INSERT INTO tenants (namespace, name) VALUES ('$(AGENT_NAMESPACE)', '$(AGENT_NAMESPACE)') \
	   ON CONFLICT (namespace) DO NOTHING"; \
	WKEY="$${NEXUS_WORKER_API_KEY:-$$(gen worker)}"; \
	WHASH=$$(printf '%s' "$$WKEY" | shasum -a 256 | cut -d' ' -f1); \
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -q -c \
	  "INSERT INTO api_keys (namespace, name, key_hash, role) \
	   VALUES ('$(AGENT_NAMESPACE)', 'worker-$$(date +%Y%m%d%H%M%S)', '$$WHASH', 'worker') \
	   ON CONFLICT (key_hash) DO NOTHING"; \
	echo ""; \
	echo "  admin  key (console, CLI):  $$KEY"; \
	echo "  worker key (agents, namespace $(AGENT_NAMESPACE)): $$WKEY"; \
	echo ""; \
	echo "  Only the SHA-256 hash is stored, so these cannot be recovered."; \
	echo "  Copy the admin key into the console via the header's \"Set API key\"."; \
	echo "  Give the worker key to the agents as NEXUS_WORKER_API_KEY, then:"; \
	echo "      NEXUS_WORKER_API_KEY=$$WKEY $(COMPOSE) up -d agent-cpu agent-gpu"; \
	echo ""

# EXAMPLE selects which of examples/*/compiled.yaml to submit. ten_node_mixed is
# the default because it IS the Phase 1 exit criterion: 10 nodes, mixed CPU/GPU.
EXAMPLE ?= ten_node_mixed

# PARAMS are the run's parameters as space-separated KEY=VALUE pairs, e.g.
#   make submit-example EXAMPLE=fan_out_map PARAMS="source=demo"
# With PARAMS the run is created and followed to a terminal state; without them
# only the spec is registered, because most examples declare required params
# and a run created without them is refused (HTTP 422) rather than guessed.
PARAMS ?=

submit-example: ## Submit an example DAG, and create a run when PARAMS is set (EXAMPLE=ten_node_mixed PARAMS="k=v ...")
	@test -f examples/$(EXAMPLE)/compiled.yaml || { echo "examples/$(EXAMPLE)/compiled.yaml not found (examples/ is owned by the CLI/SDK workstream)"; exit 1; }
	@test -n "$$NEXUS_API_KEY" || { echo "NEXUS_API_KEY is unset: run 'make seed' and export the admin key it prints"; exit 1; }
	@set -eu; \
	export NEXUS_ENDPOINT='$(NEXUS_ENDPOINT)' NEXUS_NAMESPACE='$(NAMESPACE)'; \
	name=$$(awk '/^name:/ {gsub(/["\x27]/, "", $$2); print $$2; exit}' examples/$(EXAMPLE)/compiled.yaml); \
	test -n "$$name" || { echo "no top-level name: in examples/$(EXAMPLE)/compiled.yaml"; exit 1; }; \
	$(GO) run ./cli/cmd/nexusctl workflow submit examples/$(EXAMPLE)/compiled.yaml; \
	if [ -z '$(PARAMS)' ]; then \
	  echo; echo "Spec '$$name' registered. Create a run (see examples/$(EXAMPLE) for its required params):"; \
	  echo "  make submit-example EXAMPLE=$(EXAMPLE) PARAMS=\"key=value\""; \
	  exit 0; \
	fi; \
	set --; for kv in $(PARAMS); do set -- "$$@" --param "$$kv"; done; \
	$(GO) run ./cli/cmd/nexusctl run create "$$name" "$$@" --wait

# ── Backup and restore ────────────────────────────────────────────────────
# BACKUP_DIR is passed through rather than baked in, so the same targets work
# against a laptop directory and a mounted backup volume.
BACKUP_DIR ?= ./backups

# On the host database (the default) the dump runs with the host's pg_dump
# against DATABASE_URL; with NEXUS_BUNDLED_POSTGRES=1, inside the container.
BACKUP_DSN_ENV = $(if $(filter 1,$(NEXUS_BUNDLED_POSTGRES)),,DATABASE_URL='$(DATABASE_URL)')
backup: ## Dump Postgres only (see backup-all: this is HALF a backup)
	$(BACKUP_DSN_ENV) BACKUP_DIR=$(BACKUP_DIR) deploy/backup.sh

backup-artifacts: ## Mirror the MinIO result bucket only (the other half)
	BACKUP_DIR=$(BACKUP_DIR) deploy/backup-artifacts.sh

# The target to actually use. Postgres alone leaves large output_refs pointing at
# objects nobody copied, so `make backup` is deliberately NOT the complete one --
# backup-all runs both halves in the order that makes the skew safe (objects
# first, then Postgres; see deploy/backup-artifacts.sh).
backup-all: ## Back up BOTH stores in the correct order (use this one)
	$(BACKUP_DSN_ENV) BACKUP_DIR=$(BACKUP_DIR) deploy/backup-all.sh

# Uses scratch buckets and a scratch database; never touches live data. Writes
# phase timings as JSON (DRILL_REPORT). RTO=1 adds the DESTRUCTIVE timed
# full-stack recovery (drop DB + bucket, restore, restart, reconcile) -- run it
# on a drill stack, see OPERATIONS §4.5.
DRILL_REPORT ?= ./restore-drill-report.json
restore-drill: ## Prove a restore recovers artifacts byte-identically; RTO=1 times a full recovery (DESTRUCTIVE)
	deploy/restore-drill.sh --report $(DRILL_REPORT) $(if $(filter 1,$(RTO)),--rto --yes,)

# ── Point-in-time recovery (pgBackRest; OPERATIONS §4) ────────────────────
# Needs the PITR overlay: `make up-pitr` (compose) or postgres.pitr.enabled
# (Helm). Managed Postgres uses the provider's PITR instead.
#
# The documented exception to "Postgres comes from the host": continuous WAL
# archiving and an in-place restore need a server whose data directory,
# archive_command and restarts the stack controls, so the PITR overlay always
# runs on the bundled container (never the host's server), as do the
# backup/restore drills built on it.
COMPOSE_PITR ?= $(COMPOSE_BUNDLED) -f deploy/docker-compose.pitr.yml
BACKUP_TYPE  ?= full
PITR_REPORT  ?= ./pitr-drill-report.json

up-pitr: ## Bring up the stack with continuous WAL archiving + base backups (pgBackRest)
	@deploy/scripts/agent-workdir.sh >/dev/null
	$(COMPOSE_PITR) up --build -d

backup-base: ## Take a pgBackRest base backup now (BACKUP_TYPE=full|diff|incr)
	deploy/pitr-backup.sh $(BACKUP_TYPE)

pitr-verify: ## Check archiving, backup age and repository integrity; nonzero if broken
	deploy/pitr-verify.sh

# TARGET_TIME='2026-09-24 14:31:59+00' | TARGET_LSN=0/5A0012F8 | neither = latest.
# Stop the Nexus services first; the script refuses without CONFIRM=yes.
restore-pitr: ## Restore Postgres in place to TARGET_TIME / TARGET_LSN / latest (CONFIRM=yes)
	@test "$(CONFIRM)" = yes || { echo "restore-pitr rewrites the data directory: re-run with CONFIRM=yes"; exit 1; }
	deploy/restore-pitr.sh $(if $(TARGET_TIME),--time '$(TARGET_TIME)',$(if $(TARGET_LSN),--lsn $(TARGET_LSN),--latest)) --yes

# DESTRUCTIVE to the stack it runs on (seeds data, rewinds the DB, deletes the
# data directory in scenario 2). Sizes: PITR_DRILL_ARGS="--runs-a 3000 ...".
pitr-drill: ## Timed PITR drill: restore to a timestamp and to latest; asserts + RTO/RPO JSON (DESTRUCTIVE)
	deploy/pitr-drill.sh --yes --report $(PITR_REPORT) $(PITR_DRILL_ARGS)

# ── Artifact GC (ADR-0008) ────────────────────────────────────────────────
# The restore procedure in OPERATIONS §7.2 deliberately produces orphan objects
# -- objects no database row references -- and until now nothing reclaimed them.
#
# There is NO delete target here, and that is deliberate. `make artifact-gc`
# reports; deleting requires typing the two flags out in full, so a destructive
# run is always an explicit act and never something a tab-completed make target
# can do by accident.
ARTIFACT_GC_ENDPOINT ?= http://localhost:$(or $(NEXUS_MINIO_PORT),9010)
# The same database seed and migrate act on, rather than a second copy of the
# compose URL that could drift from NEXUS_POSTGRES_PORT.
ARTIFACT_GC_DSN      ?= $(DATABASE_URL)

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

# ── Air-gapped installation (ADR-0025 §3, docs/AIRGAP.md) ─────────────────
# The bundle is built on a CONNECTED host and carried across the gap:
# digest-pinned images, the packaged chart, nexusctl/nexus-agent binaries, the
# SDK wheel, schemas/, manifest.json + SHA256SUMS (cosign-signed when cosign is
# installed). PLATFORMS=linux/amd64,linux/arm64 builds both; the default is the
# Docker host's platform.
AIRGAP_VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PLATFORMS        ?=
airgap-bundle: ## Build dist/nexus-airgap-$(AIRGAP_VERSION).tar.gz for an offline install (needs Docker, helm, go)
	python3 deploy/airgap/build-bundle.py --version '$(AIRGAP_VERSION)' $(if $(PLATFORMS),--platforms '$(PLATFORMS)')

airgap-test: ## Load the bundle into a throwaway local registry and prove every chart image resolves by digest (needs Docker, helm)
	deploy/airgap/test-load.sh 'dist/nexus-airgap-$(AIRGAP_VERSION).tar.gz'

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
# So are the `nexusctl init` scaffolds (cli/internal/templates/files/): they are
# copied into the user's own project, where our header would mislabel their code.
license-check: ## Assert every first-party source file carries an SPDX identifier
	@# testdata/upstream/ holds byte-for-byte, sha256-pinned copies of upstream
	@# example DAGs (Apache-2.0, original ASF headers kept; see each README), not first-party code.
	@missing=0; \
	for f in $$(git ls-files '*.go' '*.ts' '*.tsx' '*.py' '*.sh' '*.sql' '*.proto' \
	            | grep -v '/gen/' | grep -v '^gen/' \
	            | grep -v '^cli/internal/templates/files/' \
            | grep -v '^cli/internal/importer/testdata/upstream/'); do \
	  if ! head -5 "$$f" | grep -q 'SPDX-License-Identifier'; then \
	    echo "missing SPDX header: $$f"; missing=1; \
	  fi; \
	done; \
	if [ $$missing -eq 0 ]; then echo "SPDX headers: OK"; else exit 1; fi

# The previous release's database integration suite against THIS tree's schema,
# reached by the real upgrade path (previous migrations, then these). ADR-0011
# rule 5 says old replicas survive the new schema; this is the test that says
# so. Two throwaway databases on the host's PostgreSQL (NEXUS_TEST_PG_SERVER),
# dropped afterwards; never a Postgres container. docs/COMPATIBILITY.md §7.
.PHONY: compat-smoke
compat-smoke: ## Run the previous release's DB integration suite against this tree's migrated schema
	tests/compat/previous-release-smoke.sh

# ── Pinned tools, chart and proto checks ──────────────────────────────────
# Every tool is one exact release, downloaded into .tools/ and refused unless
# its SHA-256 matches the pin in deploy/scripts/pinned-tool.sh.
.PHONY: tools helm-validate proto-lint proto-breaking proto-check actionlint preflight-test shellcheck
tools: ## Install the pinned, checksum-verified helm, kubeconform and buf into .tools/
	@for t in helm kubeconform buf; do deploy/scripts/pinned-tool.sh $$t; done

helm-validate: ## Lint, render and schema-check the Helm chart offline-capable (deploy/helm/validate.sh)
	deploy/helm/validate.sh

proto-lint: ## buf lint over proto/ (exceptions in proto/buf.yaml)
	proto/check.sh lint

proto-breaking: ## buf breaking against the previous main (BUF_AGAINST=<rev> overrides)
	proto/check.sh breaking

proto-check: ## buf lint and buf breaking
	proto/check.sh all

# actionlint in its digest-pinned image, over .github/workflows (needs Docker).
# The ONE pin of it: `make shellcheck` runs the shellcheck it bundles, and CI
# runs both targets, so the two can never drift.
ACTIONLINT_IMAGE ?= rhysd/actionlint:1.7.12@sha256:b1934ee5f1c509618f2508e6eb47ee0d3520686341fec936f3b79331f9315667
actionlint: ## Lint the GitHub workflows with the pinned actionlint image
	docker run --rm -v "$(CURDIR):/repo:ro" -w /repo $(ACTIONLINT_IMAGE) -color

# The AKS install tooling. The harness runs deploy/scripts/aks-preflight.sh
# against a stub kubectl and az: no cluster, no network, nothing created.
preflight-test: ## Test deploy/scripts/aks-preflight.sh against stub kubectl and az (no cluster)
	deploy/scripts/tests/aks-preflight-test.sh

# Shell scripts held to shellcheck (warnings and above fail; an `info` must be
# fixed or disabled inline with the reason).
SHELLCHECK_FILES ?= deploy/scripts/aks-preflight.sh deploy/scripts/tests/aks-preflight-test.sh \
	deploy/scripts/tests/aks-preflight-stub/kubectl deploy/helm/validate.sh \
	deploy/scripts/agent-workdir.sh
shellcheck: ## shellcheck the AKS install scripts and validate.sh with the pinned actionlint image's shellcheck
	docker run --rm -v "$(CURDIR):/repo:ro" -w /repo --entrypoint shellcheck $(ACTIONLINT_IMAGE) $(SHELLCHECK_FILES)
