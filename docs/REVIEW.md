# REVIEW.md — Molot vs BOOK_AUDIT §10

## Сводка

| Уровень | Кол-во |
|---------|--------|
| Blocker | 1 |
| Major | 5 |
| Minor | 5 |
| **Итого findings** | **11** |

Правила §10 (и смежные §11), проверенные как **чистые**: layers (rules 1–3, 38), domain-auction (rules 8–16, 20–23, 40–41, 47), domain-billing/settlement (rules всего §2.2/§6), repository (rules 15–16, 18–23), security (rules 21–25), CQRS (rules 26–33), events/saga (rules 34–38).

---

## Findings по severity

### BLOCKER

#### B-1 — Rule 45: E2E-уровень полностью отсутствует

- **Файл:** `tests/` (директория `tests/e2e` не существует)
- **Суть:** E2E-уровень декларирован в `docs/test-taxonomy.md` (строка 11: `| **E2E** | … | make test-e2e |`), но не реализован ни одним файлом. Нет директории `tests/e2e`, нет build-тега `e2e`, нет Makefile-тарджета `test-e2e`.
- **Evidence:** Glob `tests/e2e*` — 0 файлов. Makefile не содержит строки `test-e2e`. `docs/test-taxonomy.md` строка 11 объявляет уровень с командой `make test-e2e`.
- **Fix:** (1) Создать `tests/e2e/critical_path_test.go` с `//go:build e2e`, покрывающий spec-flow: регистрация → лот → ставки → закрытие → оплата — против прод-бинарей через docker-compose, только публичный HTTP (переиспользовать `tests/client.go` codegen-клиент). (2) Добавить Makefile-тарджет `test-e2e`: `$(COMPOSE) up -d --build --wait`, затем `go test -race -tags e2e ./tests/... -count=1`. (3) Добавить job `e2e` в `.github/workflows/ci.yml` как merge-гейт. Одновременно закрывает major B-2 (rule 46).

---

### MAJOR

#### M-1 — Rule 39: Makefile не содержит тарджетов `test-component` и `test-e2e`

- **Файл:** `Makefile:1`
- **Суть:** `docs/test-taxonomy.md` объявляет 5 уровней тестирования с отдельными командами (`make test-component`, `make test-e2e`). Makefile содержит только `test` и `test-integration`. Merge-гейт CI неполный.
- **Evidence:** Makefile содержит ровно два тест-тарджета: `test` и `test-integration`. Тарджеты `test-component` и `test-e2e` в файле отсутствуют, хотя `docs/test-taxonomy.md` колонка «Команда» явно их указывает.
- **Fix:** Добавить `test-component` в Makefile по образцу `test-integration`: `$(COMPOSE) up -d --wait postgres`, затем `COMPONENT_DATABASE_URL=postgres://molot:molot@localhost:5432/molot_component?sslmode=disable go test -race -tags component ./tests/... -count=1` (конфигурация задокументирована в `tests/component/main_test.go:5-9`). Добавить соответствующий job `component` в `.github/workflows/ci.yml`. Тарджет `test-e2e` закрывается совместно с B-1.

#### M-2 — Rule 46: Component и E2E тесты не являются merge-гейтом CI

- **Файл:** `Makefile:22`
- **Суть:** `make test` (строка 22) запускает `go test -race ./... -count=1` без тегов `component` или `e2e`. Rule 46 требует, чтобы все уровни тестирования были в CI как merge-гейт. Комментарий в `ci.yml:2` о соответствии rule 46 фактически ложный.
- **Evidence:** `make test` строка 22: `go test -race ./... -count=1` — без тегов. Тарджеты `test-component` и `test-e2e` в Makefile отсутствуют. Тег `component` не включён ни в один CI-job.
- **Fix:** (1) Добавить Makefile-тарджеты `test-component` и `test-e2e` (см. M-1 и B-1). (2) Добавить jobs `component` и `e2e` в `.github/workflows/ci.yml` как merge-гейт (`component` — по образцу job `integration` с тегом `component`; `e2e` — поверх docker-compose). (3) До закрытия — исправить ложный комментарий `ci.yml:2`.

#### M-3 — Rule 49: `ClosingWorker` не имеет метрик `molot_worker_tick_duration` и `molot_worker_due_backlog`

- **Файл:** `internal/auction/ports/worker.go:32`
- **Суть:** §11 требует метрики `molot_worker_tick_duration{worker}` и `molot_worker_due_backlog{worker}` для **обоих** воркеров. Метрики реализованы только в `ExpiryWorker` (`internal/billing/ports/worker.go:75-80`). `ClosingWorker` не содержит соответствующих полей и не импортирует `go.opentelemetry.io/otel/metric`.
- **Evidence:** Struct `ClosingWorker` в `internal/auction/ports/worker.go` не содержит полей типа `metric.Float64Histogram` / `metric.Int64Gauge`. Grep по репозиторию: `molot_worker_tick_duration` встречается только в `ExpiryWorker`. §11: `molot_worker_tick_duration{worker}, molot_worker_due_backlog{worker} | оба воркера`.
- **Fix:** Добавить `meterProvider metric.MeterProvider` как параметр `NewClosingWorker` (в `internal/auction/service/service.go:55` `deps.MeterProvider` уже есть и валидируется в строке 80). Создать `molot_worker_tick_duration` (Float64Histogram, unit `"s"`) и `molot_worker_due_backlog` (Int64Gauge) из meter `"molot/internal/auction/ports"`. В `tick()` записывать длительность через defer + backlog через `Record(len(ids))` с атрибутом `worker="closing"`. Обновить `worker_test.go` для передачи noop meter provider.

#### M-4 — Rule 48: `ClosingWorker` не создаёт OTel-спан `worker/closing.tick`

- **Файл:** `internal/auction/ports/worker.go:86`
- **Суть:** §11 явно указывает спан `worker/closing.tick` как создаваемый воркером. Метод `tick` (строка 86) не содержит ни одного вызова `tracer.Start` / `span`. Импорты — только `context`, `log/slog`, `time`, `auction/app/command`, `decorator`.
- **Evidence:** В `internal/auction/ports/worker.go` нет импорта `go.opentelemetry.io/otel/trace`. §11 («Спаны»): `worker/closing.tick, worker/expiry.tick → дочерние commands/CloseAuction | воркер`.
- **Fix:** В `ClosingWorker.tick` в начале метода добавить `ctx, span := otel.Tracer("molot/internal/auction/ports").Start(ctx, "worker/closing.tick"); defer span.End()`. Фиксировать ошибку сканирования на спане (по образцу `recordSpanResult` в `internal/common/decorator/decorator.go:168`). Передавать span-ctx в `scanner.DueForClosing` и `closeAuction.Handle`. Аналогичный фикс применить к `ExpiryWorker.tick` в `internal/billing/ports/worker.go` (спан `"worker/expiry.tick"`) — закрывается совместно с minor M5-2.

#### M-5 — Rule 49: Метрика `molot_psp_calls_total{op,result}` не существует

- **Файл:** `internal/billing/adapters/psp_fake.go:1`
- **Суть:** §11 требует метрику `molot_psp_calls_total{op,result}` от PSP-адаптера. Ни в одном файле проекта нет этой метрики. Единственный PSP-адаптер `FakePSP` не содержит импортов `go.opentelemetry.io/otel/metric`.
- **Evidence:** Grep по всему репозиторию: 0 совпадений для `molot_psp_calls_total` и `psp_calls`. §11: `molot_psp_calls_total{op,result} + duration | psp-адаптер | decline/unavailable rate; алерт на unavailable`.
- **Fix:** Добавить инструментацию по образцу `settlement_pg_repository.go:45`. Чище всего — обёртка-декоратор `InstrumentedPSP` поверх интерфейса `command.PaymentProvider`, применяемая в `service.go:86`. Meter `"molot/internal/billing/adapters"`: `Int64Counter "molot_psp_calls_total"` с атрибутами `op=charge|refund`, `result=success|declined|unavailable|error` (классификация по `errors.Is(invoice.ErrPaymentDeclined / invoice.ErrPSPUnavailable)`) + `Float64Histogram "molot_psp_call_duration_seconds"`. Добавить спаны `psp.Charge` / `psp.Refund` с атрибутом `idempotency_key` (§11, ADR-0005). `MeterProvider` / `TracerProvider` в billing `Deps` уже есть.

---

### MINOR

#### m-1 — Rule 40: Domain unit tests используют `testify/assert.Equal` вместо `go-cmp + AllowUnexported`

- **Файл:** `internal/billing/domain/invoice/invoice_test.go:1`
- **Суть:** Rule 40 предписывает `go-cmp + AllowUnexported` для сравнения структур с приватными полями. Ни в одном domain test file (`invoice_test.go`, `money_test.go`, `participant_test.go`, `settlement_test.go`, `decline_test.go`, `vo_test.go`) нет импорта `github.com/google/go-cmp/cmp`. Тесты сравнивают значения только через exported getters, обходя требование инструмента.
- **Evidence:** Grep по всем domain test файлам: 0 импортов `github.com/google/go-cmp/cmp`. Сравнения полных структур выполняются через `assert.Equal` на геттерах, не на самих структурах.
- **Fix:** Два варианта соответствия. (a) Принять предписанный инструмент: добавить `github.com/google/go-cmp` как прямую зависимость, заменить whole-struct `assert.Equal` в domain тестах вспомогательным хелпером с `cmp.AllowUnexported(invoice.InvoiceID{}, invoice.Money{}, invoice.Currency{}, invoice.PaymentReference{}, invoice.InvoiceStatus{}, invoice.Attempt{})` и аналогично для settlement/participant VO. Скалярные getter-ассерты можно оставить на testify. (b) Задокументировать исключение — добавить строку в `docs/test-taxonomy.md` (и поправить BOOK_AUDIT rule 40), что `testify` deep-equality принят для VO-сравнений, поскольку все domain VO — comparable single-field wrappers; зафиксировать как намеренное отклонение. Вариант (b) дешевле и соответствует существующей проектной конвенции.

#### m-2 — Rule 40: `TestDecidePhase` не покрывает `DecideOnPaymentReceived`

- **Файл:** `internal/settlement/domain/settlement/settlement_test.go:275`
- **Суть:** `TestDecidePhase` покрывает `DecideOnInvoiceIssue`, `DecideOnPaymentTimeout`, `DecideOnWinnerReassigned`, но `DecideOnPaymentReceived` (`settlement.go:122`) не имеет ни одного test case. `StateStarted` fast-forward через `guardLiveInvoice` проверяется только в commit-phase тесте `PaymentReceived` (строки 75–83), но не на уровне decide-phase guard.
- **Evidence:** В `settlement_test.go` функция `TestDecidePhase` (строка 275) не содержит вызовов `s.DecideOnPaymentReceived`. ARCHITECTURE.md §12 указывает на 14 состояний саги.
- **Fix:** Добавить table-driven subtest в `TestDecidePhase` (`internal/settlement/domain/settlement/settlement_test.go`) с прямым вызовом `s.DecideOnPaymentReceived`: (a) `startedSaga + s.InvoiceID()` → `nil` (fast-forward seam, §6.3); (b) `awaitingSaga + s.InvoiceID()` → `nil`; (c) `awaitingSaga + newInvoiceID(t)` → `ErrUnexpectedTransition`; (d) `settledSaga + s.InvoiceID()` → `ErrUnexpectedTransition`. Стиль — табличный, по образцу `DecideOnPaymentTimeout`. Новые fixtures не нужны, ~10 строк.

#### m-3 — Rule 37: `TestOnParticipantVerified_IsNoOp` не содержит redelivery-ассерции

- **Файл:** `internal/notification/ports/events_test.go:540`
- **Суть:** Rule 37 требует явного теста идемпотентности каждого handler. `TestOnParticipantVerified_IsNoOp` вызывает handler один раз и проверяет пустые результаты. Все остальные handler-тесты в том же файле содержат явный повторный вызов, помеченный `// redelivery` или `// at-least-once redelivery`.
- **Evidence:** `TestOnParticipantVerified_IsNoOp` (строки 540–549): один вызов `f.handle(...)`, после — `assert.Empty` на emails и rows. Ни одного второго вызова нет. Все прочие handler-тесты файла содержат повторный вызов с redelivery-комментарием.
- **Fix:** В `TestOnParticipantVerified_IsNoOp` (`internal/notification/ports/events_test.go:546`) добавить второй вызов перед ассерциями: `require.NoError(t, f.handle(t, "OnParticipantVerified", event)) // at-least-once redelivery`. Существующие `assert.Empty` непосредственно подтвердят, что redelivery ничего не добавляет.

#### m-4 — Rule 49: Метрика `molot_bus_retries_total{handler}` не реализована в Retry middleware

- **Файл:** `internal/common/watermill/watermill.go:59`
- **Суть:** §11 требует счётчик `molot_bus_retries_total{handler}` в Retry middleware. Grep по всему репозиторию: 0 совпадений. Retry-middleware в `watermill.go` (строки 59–66) создаётся без какой-либо инструментации.
- **Evidence:** Grep по всему репозиторию: 0 результатов для `molot_bus_retries_total` и `bus_retries`. §11: `molot_bus_retries_total{handler} | Retry middleware | ранний сигнал деградации`.
- **Fix:** Добавить `Int64Counter "molot_bus_retries_total{handler}"` и инкрементировать на каждую retry-попытку. Подход: передать `metric.MeterProvider` в `NewRouter` (`internal/monolith/app.go:197` уже имеет его), создать счётчик, добавить small middleware после `retry.Middleware` (внутри Retry, до Recoverer), инкрементирующий счётчик с атрибутом `handler=message.HandlerNameFromCtx(msg.Context())` при возврате ошибки. Альтернатива через `Retry.OnRetryHook` не даёт имя handler, поэтому wrapper-middleware предпочтительнее для поддержки label `{handler}`.

#### m-5 — Rule 48: `ExpiryWorker` не создаёт OTel-спан `worker/expiry.tick`

- **Файл:** `internal/billing/ports/worker.go:113`
- **Суть:** §11 требует спан `worker/expiry.tick` от `ExpiryWorker`. Метод `tick` (строка 113) импортирует `go.opentelemetry.io/otel/attribute` и `go.opentelemetry.io/otel/metric`, но не `trace`. Вызовов `tracer.Start` нет.
- **Evidence:** В `internal/billing/ports/worker.go` нет импорта `go.opentelemetry.io/otel/trace`. §11 («Спаны»): `worker/closing.tick, worker/expiry.tick → дочерние commands/CloseAuction | воркер`.
- **Fix:** Добавить `trace.TracerProvider` как параметр `NewExpiryWorker` (рядом с существующим `metric.MeterProvider`). В начале `tick()`: `ctx, span := w.tracer.Start(ctx, "worker/expiry.tick"); defer span.End()`. `TracerProvider` уже присутствует в `internal/billing/service/service.go`. Аналогичный фикс для `ClosingWorker` в `internal/auction/ports/worker.go` (спан `"worker/closing.tick"`) — покрывается совместно с major M-4.

---

## Проверено и чисто

Следующие области проверены, нарушений не обнаружено:

**Layers (rules 1–3, 38):** Все domain-пакеты (auction, billing, participant, settlement) — ноль импортов других слоёв или внешней инфры. Ports-пакеты не импортируют adapters. Кросс-контекстный sync-facade (settlement/adapters → auctionservice/billingservice) соответствует ADR-0004 и rule 38. Пакет `common/` — только stdlib и другие common-подпакеты.

**Domain-auction (rules 8–16, 20–23, 40–41, 47):** Все поля приватны. Валидирующие конструкторы отклоняют zero/blank. Методы поведения (`PlaceBid`, `Cancel`, `Close`, `AwardToRunnerUp`, `ConfirmSettlement`, `FailSale`) — guard + атомарный переход без сеттеров. Порядок guard cascade в `PlaceBid` точно соответствует ARCHITECTURE §2.1. Sentinel-ошибки — exported package-level vars. Нет db/json тегов на доменных типах. Enum VO с `IsZero()`, `NewXFromString`. `MinimalNextBid` / `CanSellerManageAuction` — stateless package-level functions. Репозиторий-интерфейс минимален. Все мутации через `updateFn`-замыкание. Идемпотентность фасадов корректна. Inmem-репо хранит map значений, возвращает `&cp`. Нет `fmt.Println`/log в domain.

**Domain-billing/settlement (§2.2/§6):** Все 12 ячеек Invoice guard table соответствуют spec. Settlement state machine покрывает все 7 состояний. Decide-phase — value receivers, commit-phase — pointer receivers. `CanRunnerUpDecline` — корректная pure package-level function. `nextStepOnFailure` compensation fork соответствует spec. `UnmarshalFromDatabase` factories присутствуют. Все enum VO с `IsZero()` и switch default:panic (кроме одного minor в `Participant.Verify`).

**Repository (rules 15–16, 18–23):** Все четыре pg-репозитория и четыре inmem-реализации. Repository-интерфейсы в domain-пакетах, минимальны. `updateFn`-контракт: RunInTx → SELECT FOR UPDATE → load → updateFn → persist. Named return + defer `FinishTransaction`. UTC нормализация. `sql.ErrNoRows` маппируется в domain-ошибки. Outbox в той же транзакции. `UpdateAsSystem` / `UpdateAsOperations` не используют fake users.

**Security (rules 21–25):** `auction.Actor`, `invoice.BidderID`, `participant.Actor` — явные типизированные параметры, `ctx.Value` для identity не используется нигде в бизнес-слое. `CanDebtorAccessInvoice` и `CanActorUpdateParticipant` вызываются в адаптерах до `updateFn`. `UpdateAsSystem` / `UpdateAsOperations` именованы корректно. JWT: `golang-jwt/jwt/v5` с `WithValidMethods` и `WithExpirationRequired`. Анти-enumeration инвойсов реализован корректно в billing.

**CQRS (rules 26–33):** Все команды возвращают только `error`. Query-результаты — плоские UI-shaped типы. Имена — бизнес-язык. Порты зовут только `app.Application.Commands/Queries.X.Handle`. Декораторы применяются в `service/`. Единый `httperr.RespondWithSlugError`. `ListAuction` / `PlaceBid` / `RegisterParticipant` → 204+content-location. Read models на том же Postgres. Notification: отсутствие domain/app-слоёв задокументировано в `internal/notification/README.md`.

**Events/Saga (rules 34–38):** Events-пакеты — плоские версионированные V1-structs. Outbox в той же транзакции. Router middleware в точном порядке: CorrelationID → PoisonQueue → Retry{5, exp backoff 30s} → Recoverer. Audit projection: upsert + monotonic bid_count guard. Notification: `SentLog.FirstDelivery`. Saga: все 5 commit-точек вызывают gateway-методы строго вне `repo.Update`. Идемпотентность фасадов: все 6 sentinel-маппингов →nil. Кросс-контекстные интерфейсы (`auctionGateway`, `billingGateway`) в settlement/app с compile-time proof в settlement/adapters.

---

## Рекомендованный порядок фиксов

1. **B-1 (rule 45) + M-2 (rule 46) + M-1 (rule 39)** — реализовать E2E-тесты и добавить Makefile-тарджеты `test-component` / `test-e2e` с CI-jobs. Закрывает разрыв между задекларированным и реальным тест-контрактом; CI-комментарий о соответствии rule 46 перестаёт быть ложным.

2. **M-5 (rule 49, psp_calls_total) + M-4 (rule 48, worker/closing.tick) + m-5 (rule 48, worker/expiry.tick)** — обе observability-проблемы решаются за одну итерацию: добавить `InstrumentedPSP`-декоратор и спаны воркеров. `MeterProvider` / `TracerProvider` в deps уже есть.

3. **M-3 (rule 49, ClosingWorker metrics) + m-4 (rule 49, bus_retries_total)** — оставшиеся метрики; ClosingWorker — шаблонный порт из ExpiryWorker, bus_retries — small middleware.

4. **m-2 (rule 40, DecideOnPaymentReceived)** — table-driven subtest ~10 строк, нет новых fixtures.

5. **m-3 (rule 37, redelivery assert)** — одна строка в тесте.

6. **m-1 (rule 40, go-cmp vs testify)** — выбрать вариант (a) или (b) и зафиксировать решение в `docs/test-taxonomy.md`.
