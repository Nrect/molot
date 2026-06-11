# Molot — a reference Go business system

> English | [Русский](README.ru.md)

An auction platform (English auction) built as a **reference for long-lived business systems**: modular monolith, DDD + CQRS + Clean Architecture, event-driven integration over a transactional outbox, a saga with compensations, and end-to-end observability. The architecture is distilled from *Go With The Domain* (Three Dots Labs) and pinned down by a 55-rule contract.

> The project is deliberately "teaching-grade production": every decision here is meant to be a template for real systems with heavy business requirements. Read top-down: documents → domain → everything else.

## How to read this repository

1. **[docs/BOOK_AUDIT.md](docs/BOOK_AUDIT.md)** — the contract: 55 imperative architecture rules (layers, DDD tactics, repositories, CQRS, events, tests, observability). Everything is reviewed against it. *(Russian; identifiers and pattern names in English.)*
2. **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — the working spec: context map, aggregates and invariants, command/query catalog, events, the saga (transition table, idempotency, crash-seam map), DB schema, HTTP API, time-based workers.
3. **[docs/event-storming.md](docs/event-storming.md)** — the domain flow and the "event/command → Go type/handler" mapping.
4. **[docs/adr/](docs/adr/)** — decisions with trade-offs: modular monolith, Postgres-backed bus (watermill-sql), the Auction aggregate boundary, the settlement saga, observability.
5. **Domain code** — start with [internal/auction/domain/auction](internal/auction/domain/auction) and [internal/settlement/domain/settlement](internal/settlement/domain/settlement) (the saga state machine).
6. **[docs/REVIEW.md](docs/REVIEW.md)** — implementation review against the contract; [docs/test-taxonomy.md](docs/test-taxonomy.md) — test levels; [docs/operations.md](docs/operations.md) — runbook (rollback, dead-letter redelivery); [docs/HIGHLOAD.md](docs/HIGHLOAD.md) — high-load patterns mapped to this codebase; [docs/ROADMAP.md](docs/ROADMAP.md) — prioritized candidates for next iterations.

## The domain in a nutshell

A seller lists a lot → participants bid (bid increment rules, anti-sniping extends the window, the reserve price stays hidden) → the auction closes by time → the winner gets an invoice (hammer price + platform commission) → payment through a PSP → if the payment times out: a second-chance offer to the runner-up OR an automatic relist (capped at one generation) → the settlement completes or fails. Every step after the hammer is orchestrated by the settlement saga with idempotent compensations.

## Bounded contexts

| Context | Responsibility | Highlights |
|---|---|---|
| `auction` | Lots, bidding, closing, catalog views | Aggregate keeps the top-2 bids denormalized + an append-only bid history inside the consistency boundary; optimistic locking; anti-sniping; the PlaceBid vs Close race is settled by a row lock + aggregate guards |
| `billing` | Invoices, PSP, payment timeout | Charge before the transaction + idempotent Refund (the charged-but-expired race is closed); an exhaustive invoice transition guard table; anti-enumeration (someone else's invoice = 404) |
| `settlement` | The saga | decide → effect → commit protocol; fast-forward on out-of-order events; a unit test for every crash seam |
| `participant` | Registration, verification | A small context that is deliberately NOT over-engineered |
| `notification` | Events → email | A trivial consumer without layers — a documented exception |

Context integration: events through a **transactional outbox** (watermill-sql: Postgres as the bus; Kafka later = swapping the publisher/subscriber, ADR-0002) + synchronous facades for saga commands (consumer-side interfaces, ADR-0004).

## Running it

```bash
cp .env.example .env          # defaults are fine for local use
docker compose up -d          # postgres + app + otel-collector + jaeger + prometheus + grafana
# or a local binary against the compose postgres:
make run
```

| Service | URL |
|---|---|
| API | http://localhost:8080 (healthz/readyz; business endpoints under `/api`, JWT HS256) |
| Jaeger (traces) | http://localhost:16686 — the "hammer → invoice → payment → settlement" path is a single trace |
| Prometheus | http://localhost:9090 |
| Grafana (dashboards) | http://localhost:3000 — "Molot — Contexts RED", "Molot — Bus & Saga" |

## Tests

```bash
make test               # unit (domain + app), -race, no docker required
make test-integration   # adapters against a real Postgres
make test-component     # the whole application in-process, fake PSP
make test-e2e           # production binaries from docker compose, public HTTP only
```

The level taxonomy lives in [docs/test-taxonomy.md](docs/test-taxonomy.md). Mandatory patterns: a rollback test for every transactional repository, race tests ("20 goroutines, exactly one winner"), idempotency of every event handler proven by redelivery, saga crash-seam continuation tests.

## Layout

```
cmd/monolith/            thin main: process concerns, signals, the HTTP listener
internal/monolith/       composition root (prod + component tests → one shared newApplication)
internal/common/         infrastructure only: CQRS decorators (logs+RED+spans), errs, auth,
                         postgres (RunInTx/FinishTransaction), watermill (router, outbox, bus metrics)
internal/<context>/
  domain/<aggregate>/    stdlib-only: invariants, behavior methods, sentinel errors, Repository interface
  app/{command,query}/   use cases; consumer-side dependency interfaces
  ports/                 HTTP (oapi-codegen strict), event handlers, workers
  adapters/              Postgres/in-memory repos, events_mapper (outbox in the same tx), goose migrations
  events/                flat versioned V1 contracts — the only package importable from outside
  service/               context assembly + the facade for the saga
api/openapi/             contracts (codegen via make openapi)
tests/                   component + e2e
```

Dependency direction is CI-enforced: domain ← app ← ports/adapters; importing another context's `domain/` is forbidden; `common` holds zero business types.

## What's next

The full prioritized candidate list is **[docs/ROADMAP.md](docs/ROADMAP.md)** (11 sections, ~85 items). Briefly:

- SLO burn-rate alerts on top of the existing metrics; k6 load tests (concurrent bidding); chaos via Toxiproxy.
- A double-entry ledger in billing; supply-chain CI (govulncheck/trivy/SBOM/cosign).
- Kafka instead of watermill-sql — swap the publisher/subscriber + forwarder, contracts stay (ADR-0002).
- Extracting a context into a service — events are already versioned, the facade becomes a gRPC adapter.
- Kubernetes (helm + kind in CI), a release pipeline, backups with restore drills.
