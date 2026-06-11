# Molot — dev targets. Deliberately plain: go + docker compose only,
# so the same commands work locally (POSIX sh / Git Bash on Windows) and in CI.

COMPOSE := docker compose
TEST_DATABASE_URL ?= postgres://molot:molot@localhost:5432/molot?sslmode=disable
COMPONENT_DATABASE_URL ?= postgres://molot:molot@localhost:5432/molot_component?sslmode=disable
E2E_BASE_URL ?= http://localhost:8080

.PHONY: openapi lint test test-integration test-component test-e2e up down run

# Regenerate strict-server code from OpenAPI specs (.gen.go is never edited by hand).
# Run from the repo root: output paths in cfg-*.yaml are CWD-relative.
openapi:
	go tool oapi-codegen -config api/openapi/cfg-auction.yaml api/openapi/auction.yaml
	go tool oapi-codegen -config api/openapi/cfg-participant.yaml api/openapi/participant.yaml
	go tool oapi-codegen -config api/openapi/cfg-billing.yaml api/openapi/billing.yaml
	go tool oapi-codegen -config api/openapi/cfg-settlement.yaml api/openapi/settlement.yaml

lint:
	go vet ./...
	golangci-lint run

test:
	go test -race ./... -count=1

# Integration level: real Postgres from compose; tests are gated by the
# `integration` build tag and skip themselves when TEST_DATABASE_URL is unset.
test-integration:
	$(COMPOSE) up -d --wait postgres
	TEST_DATABASE_URL=$(TEST_DATABASE_URL) go test -race -tags integration ./... -count=1

# Component level: the whole app in one process against real Postgres from
# compose; gated by the `component` build tag and skipped when
# COMPONENT_DATABASE_URL is unset. The URL is an ADMIN entry point (see
# tests/component/main_test.go): the suite creates and drops its own
# uniquely named databases, so the molot_component entry database only has
# to exist — created idempotently below.
test-component:
	$(COMPOSE) up -d --wait postgres
	$(COMPOSE) exec -T postgres psql -U molot -d molot -tAc "SELECT 1 FROM pg_database WHERE datname = 'molot_component'" | grep -q 1 || \
		$(COMPOSE) exec -T postgres psql -U molot -d molot -c "CREATE DATABASE molot_component"
	COMPONENT_DATABASE_URL=$(COMPONENT_DATABASE_URL) go test -race -tags component ./tests/... -count=1

# E2E level: one short critical-path flow against the production binaries
# from docker compose, public HTTP only; gated by the `e2e` build tag and
# skipped when E2E_BASE_URL is unset. The stack is recreated from scratch
# (down -v): a stale dev volume carries old bus backlog/offsets that stall
# fresh projections and poison the run. SNIPE_* shrink the anti-snipe
# window for the seconds-long e2e lot (docker-compose.yml interpolates
# them into the app environment; the defaults there keep .env.example).
test-e2e:
	$(COMPOSE) down -v --remove-orphans
	SNIPE_WINDOW=1s SNIPE_EXTENSION=1s $(COMPOSE) up -d --build --wait
	E2E_BASE_URL=$(E2E_BASE_URL) go test -race -tags e2e ./tests/... -count=1

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

run:
	go run ./cmd/monolith
