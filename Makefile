# PaymentGateway monorepo Makefile
# Single source of truth for services/ports/layout: docs/01-architecture.md
#
# Conventions:
#   - Binaries are built to ./bin/<service>
#   - Dev tools are installed to ./bin (GOBIN=$(PWD)/bin), never to $GOPATH/bin
#   - Proto generation goes to api/gen/go (committed to the repo)
#   - Migrations live in migrations/<svc> where <svc> is the service short name
#     (merchant, payment, ledger, webhook, reconciliation)

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
.DELETE_ON_ERROR:
MAKEFLAGS += --no-builtin-rules --no-print-directory

# ---------------------------------------------------------------------------
# Project metadata
# ---------------------------------------------------------------------------
MODULE   := github.com/tenghongzou/paymentgateway
SERVICES := api-gateway merchant-service payment-service ledger-service webhook-service reconciliation-service provider-mock provider-stripe

# Services that own a database (and therefore a migrations/<svc> directory).
MIGRATE_SERVICES := merchant payment ledger webhook reconciliation

# Database short name per migrations dir (only reconciliation differs: pg_recon / recon_owner).
# Roles/databases are created by deploy/compose/postgres/init.sql (DBA): <short>_owner (DDL) / <short>_app (DML).
DB_SHORT_reconciliation := recon

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

BIN      := $(CURDIR)/bin
GOBIN    := $(BIN)
export GOBIN
export PATH := $(BIN):$(PATH)
export CGO_ENABLED := 0

GO        ?= go
GOFLAGS   ?=
LDFLAGS   := -s -w -X main.version=$(VERSION) -X main.commit=$(GIT_COMMIT) -X main.buildDate=$(BUILD_DATE)
GOBUILD   := $(GO) build -trimpath -ldflags "$(LDFLAGS)"
PKGS      := ./...
TEST_FLAGS ?= -race -count=1 -timeout 10m

# Docker / compose
IMAGE_PREFIX ?= paymentgateway
DOCKERFILE   := deploy/docker/Dockerfile
PLATFORM     ?=
COMPOSE_FILE := deploy/compose/docker-compose.yaml
COMPOSE      := docker compose -f $(COMPOSE_FILE)
COMPOSE_PROFILES ?=

# Migrations (make migrate-up SVC=payment)
SVC          ?= payment
DB_SHORT      = $(or $(DB_SHORT_$(SVC)),$(SVC))
DB_NAME       = pg_$(DB_SHORT)
DB_USER      ?= $(DB_SHORT)_owner
DB_PASSWORD  ?= $(DB_USER)
DB_HOST      ?= localhost
DB_PORT      ?= 5432
DATABASE_URL ?= postgres://$(DB_USER):$(DB_PASSWORD)@$(DB_HOST):$(DB_PORT)/$(DB_NAME)?sslmode=disable
MIGRATE       = $(BIN)/migrate
MIGRATE_STEPS ?=

# Proto
PROTO_DIR := api/proto
PROTO_OUT := api/gen/go

# Tool versions (override: make tools GOLANGCI_LINT_VERSION=v2.1.0)
PROTOC_GEN_GO_VERSION      ?= latest
PROTOC_GEN_GO_GRPC_VERSION ?= latest
GOLANGCI_LINT_VERSION      ?= latest
MIGRATE_VERSION            ?= latest
BUF_VERSION                ?= latest

# ---------------------------------------------------------------------------
# Help
# ---------------------------------------------------------------------------
.PHONY: help
help: ## Show this help (default target)
	@echo "PaymentGateway - make targets"
	@echo ""
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_.-]+:.*##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
	@echo ""
	@echo "Variables: VERSION=$(VERSION)  SVC=$(SVC)  IMAGE_PREFIX=$(IMAGE_PREFIX)"

##@ Tooling

.PHONY: tools
tools: $(BIN) ## Install dev tools into ./bin (protoc-gen-go, protoc-gen-go-grpc, golangci-lint, migrate, buf)
	GOBIN=$(BIN) $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN=$(BIN) $(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	GOBIN=$(BIN) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	GOBIN=$(BIN) $(GO) install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION)
	GOBIN=$(BIN) $(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	@echo "tools installed in $(BIN)"

$(BIN):
	@mkdir -p $(BIN)

##@ Protobuf

.PHONY: proto
proto: ## Generate Go code from api/proto (buf generate; falls back to protoc loop)
	@mkdir -p $(PROTO_OUT)
	@if command -v buf >/dev/null 2>&1; then \
		echo ">> buf generate"; \
		buf generate; \
	else \
		echo ">> buf not found, falling back to scripts/protoc-gen.sh"; \
		./scripts/protoc-gen.sh; \
	fi

.PHONY: proto-lint
proto-lint: ## Lint proto files with buf
	buf lint

.PHONY: proto-breaking
proto-breaking: ## Check proto breaking changes against main (BREAKING_AGAINST overrides)
	buf breaking --against '$(or $(BREAKING_AGAINST),.git#branch=main)'

.PHONY: proto-clean
proto-clean: ## Remove generated proto code
	rm -rf $(PROTO_OUT)

##@ Build & quality

.PHONY: build
build: $(BIN) ## Build all services to ./bin/<service> (static, stripped)
	@for s in $(SERVICES); do \
		echo ">> building $$s"; \
		$(GOBUILD) -o $(BIN)/$$s ./cmd/$$s; \
	done

.PHONY: $(addprefix build-,$(SERVICES))
$(addprefix build-,$(SERVICES)): build-%: $(BIN) ## Build a single service (build-payment-service)
	$(GOBUILD) -o $(BIN)/$* ./cmd/$*

.PHONY: test
test: ## Run unit tests (race)
	$(GO) test $(TEST_FLAGS) -coverprofile=coverage.out -covermode=atomic $(PKGS)

.PHONY: test-integration
test-integration: ## Run integration tests (-tags integration; needs docker for testcontainers)
	$(GO) test $(TEST_FLAGS) -tags integration -coverprofile=coverage-integration.out -covermode=atomic $(PKGS)

.PHONY: cover
cover: test ## Open HTML coverage report
	$(GO) tool cover -html=coverage.out

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format code (golangci-lint fmt -> gofmt/goimports)
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint fmt ./...; else gofmt -s -w $$(find . -name '*.go' -not -path './api/gen/*' -not -path './bin/*'); fi

.PHONY: vet
vet: ## go vet
	$(GO) vet $(PKGS)

.PHONY: tidy
tidy: ## go mod tidy + verify
	$(GO) mod tidy
	$(GO) mod verify

.PHONY: check
check: fmt vet lint test ## fmt + vet + lint + test

##@ Database migrations

.PHONY: migrate-up
migrate-up: ## Apply migrations for SVC (make migrate-up SVC=payment [MIGRATE_STEPS=1])
	@test -x $(MIGRATE) || { echo "missing $(MIGRATE); run 'make tools'"; exit 1; }
	$(MIGRATE) -path migrations/$(SVC) -database '$(DATABASE_URL)' up $(MIGRATE_STEPS)

.PHONY: migrate-down
migrate-down: ## Roll back migrations for SVC (make migrate-down SVC=payment MIGRATE_STEPS=1)
	@test -x $(MIGRATE) || { echo "missing $(MIGRATE); run 'make tools'"; exit 1; }
	$(MIGRATE) -path migrations/$(SVC) -database '$(DATABASE_URL)' down $(or $(MIGRATE_STEPS),1)

.PHONY: migrate-version
migrate-version: ## Show current migration version for SVC
	$(MIGRATE) -path migrations/$(SVC) -database '$(DATABASE_URL)' version

.PHONY: migrate-create
migrate-create: ## Create a new migration pair (make migrate-create SVC=payment NAME=add_refunds)
	@test -n "$(NAME)" || { echo "NAME is required"; exit 1; }
	$(MIGRATE) create -ext sql -dir migrations/$(SVC) -seq -digits 4 $(NAME)

.PHONY: migrate-up-all
migrate-up-all: ## Apply migrations for every service that owns a database
	@for s in $(MIGRATE_SERVICES); do \
		echo ">> migrate up: $$s"; \
		$(MAKE) migrate-up SVC=$$s; \
	done

##@ Docker & compose

.PHONY: docker-build
docker-build: ## Build one image per service: $(IMAGE_PREFIX)/<service>:$(VERSION)
	@for s in $(SERVICES); do \
		echo ">> docker build $(IMAGE_PREFIX)/$$s:$(VERSION)"; \
		docker build $(if $(PLATFORM),--platform $(PLATFORM),) \
			-f $(DOCKERFILE) \
			--build-arg SERVICE=$$s \
			--build-arg VERSION=$(VERSION) \
			--build-arg GIT_COMMIT=$(GIT_COMMIT) \
			-t $(IMAGE_PREFIX)/$$s:$(VERSION) \
			-t $(IMAGE_PREFIX)/$$s:latest \
			. ; \
	done

.PHONY: $(addprefix docker-build-,$(SERVICES))
$(addprefix docker-build-,$(SERVICES)): docker-build-%: ## Build a single image (docker-build-payment-service)
	docker build $(if $(PLATFORM),--platform $(PLATFORM),) -f $(DOCKERFILE) \
		--build-arg SERVICE=$* --build-arg VERSION=$(VERSION) --build-arg GIT_COMMIT=$(GIT_COMMIT) \
		-t $(IMAGE_PREFIX)/$*:$(VERSION) -t $(IMAGE_PREFIX)/$*:latest .

.PHONY: compose-up
compose-up: ## Start the local stack (COMPOSE_PROFILES=tools to add kafka-ui/vault)
	VERSION=$(VERSION) COMPOSE_PROFILES=$(COMPOSE_PROFILES) $(COMPOSE) up -d --build --remove-orphans

.PHONY: compose-down
compose-down: ## Stop the local stack and remove volumes
	$(COMPOSE) --profile tools down -v --remove-orphans

.PHONY: compose-logs
compose-logs: ## Tail logs (make compose-logs S=payment-service)
	$(COMPOSE) logs -f --tail=200 $(S)

.PHONY: compose-ps
compose-ps: ## Show stack status
	$(COMPOSE) ps

.PHONY: compose-infra
compose-infra: ## Start only infrastructure (postgres/redis/kafka/otel) for running services from the IDE
	$(COMPOSE) up -d postgres redis kafka otel-collector jaeger prometheus grafana

.PHONY: compose-config
compose-config: ## Validate and render the compose file
	$(COMPOSE) config -q && echo "compose file OK"

##@ End-to-end

E2E_WAIT_TIMEOUT ?= 180

.PHONY: e2e
e2e: ## Start compose stack, wait for services to be healthy, run go test -tags e2e ./test/e2e/...
	@set +e; \
	VERSION=$(VERSION) $(COMPOSE) up -d --build --wait --wait-timeout $(E2E_WAIT_TIMEOUT); rc=$$?; \
	if [ $$rc -eq 0 ]; then ./scripts/wait-for-http.sh -t $(E2E_WAIT_TIMEOUT) \
		http://localhost:8080/healthz \
		http://localhost:18001/healthz http://localhost:18002/healthz http://localhost:18003/healthz \
		http://localhost:18004/healthz http://localhost:18005/healthz \
		http://localhost:18101/healthz http://localhost:18102/healthz; rc=$$?; fi; \
	if [ $$rc -eq 0 ]; then \
		PG_E2E_GATEWAY_URL=http://localhost:8080 $(GO) test -count=1 -timeout 15m -tags e2e ./test/e2e/...; rc=$$?; \
	fi; \
	if [ $$rc -ne 0 ]; then echo ">> e2e failed, dumping logs"; $(COMPOSE) logs --no-color --tail=200; fi; \
	if [ -z "$(E2E_KEEP)" ]; then $(COMPOSE) down -v --remove-orphans; fi; \
	exit $$rc

##@ Housekeeping

.PHONY: clean
clean: ## Remove build artifacts, coverage files and tool binaries
	rm -rf $(BIN) coverage.out coverage-integration.out dist
	$(GO) clean -cache -testcache 2>/dev/null || true

.PHONY: version
version: ## Print the version that would be embedded in builds
	@echo $(VERSION)
