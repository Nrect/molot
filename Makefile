# Molot — dev targets. Deliberately plain: go + docker compose only,
# so the same commands work locally (POSIX sh / Git Bash on Windows) and in CI.

COMPOSE := docker compose
TEST_DATABASE_URL ?= postgres://molot:molot@localhost:5432/molot?sslmode=disable

.PHONY: openapi lint test test-integration up down run

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

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

run:
	go run ./cmd/monolith
