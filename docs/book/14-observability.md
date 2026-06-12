# Глава 14. Observability: три сигнала из одной точки

## Зачем читать

Тесты зелёные. CI прошёл. Деплой выполнен. А через три часа ночной дежурный видит: продавцы жалуются, что расчёты «зависли». Ни одна метрика CPU не шевельнулась. В логах — тишина. Трейсов нет.

Глава объясняет, как устроен наблюдаемый сервис на практике: почему три сигнала — логи, трейсы и метрики — генерируются из одного места в коде, а не размазаны по хендлерам; как сквозной трейс превращает четырёхконтекстную цепочку «молоток → счёт → оплата → расчёт» в одну временну́ю шкалу в Jaeger; и какие именно метрики отвечают на вопрос ночного дежурного «где и почему».

---

## Проблема: разрозненная диагностика убивает время

Типичная картина в молодом проекте: логи пишутся `fmt.Println` там, где разработчику было удобно; метрики — CPU и memory, потому что они «красивые»; трейсов нет вообще. Когда сага зависает между контекстами, дежурный читает логи трёх сервисов вручную, пытаясь восстановить причинность по timestamp-у.

Три конкретных следствия:

1. **Разрыв причинности.** HTTP-запрос выставил лот. Через минуту упал event handler в settlement. Связать их без общего trace_id — упражнение на удачу.
2. **Тихие деградации.** WaterMill retry loop исправно проглатывает ошибки. Dead-letter растёт. Дежурный узнаёт на пятой жалобе пользователя.
3. **Алерты не на симптомы.** Алерт на CPU срабатывает раз в квартал. Зависшая сага — никогда, потому что такой метрики нет.

Решение не в том, чтобы добавить больше логов в хендлеры. Решение — в архитектурном выборе: cross-cutting observability живёт в одном месте.

---

## Теория: три сигнала и их роли

**Логи** отвечают на вопрос «что произошло с конкретным запросом». Структурированный JSON с `trace_id`, `correlation_id` и `handler` превращает поиск в точечный запрос, а не в `grep` по тексту.

**Трейсы** отвечают на вопрос «где в цепочке потерялось время или появилась ошибка». Распределённый трейс — это граф спанов с временными метками; он единственный инструмент, показывающий причинность через асинхронную шину.

**Метрики** отвечают на вопрос «что происходит с системой в целом прямо сейчас». Они агрегируют тысячи запросов в одно число; по ним строятся алерты.

Ключевая идея: все три сигнала должны быть **скоррелированы**. Запись в логе несёт `trace_id`. Трейс ведёт к конкретному span-у с ошибкой. Метрика показывает, насколько распространена ошибка. Дежурный прыгает между ними в три клика, а не в три часа.

**RED-метод** (Rate / Errors / Duration) на use case — стандартный ответ на вопрос «что мониторить»: сколько запросов в секунду, сколько из них ошибочных, как долго выполняются. Он работает на уровне отдельного хендлера, а не всего сервиса целиком.

---

## Как в Molot

### Декораторы: один источник для всех трёх сигналов

Центральная идея Molot — cross-cutting concerns не в хендлерах. Каждый command/query хендлер проходит через `ApplyCommandDecorators` / `ApplyQueryDecorators` из `internal/common/decorator/decorator.go`, и получает логирование, RED-метрики и трейсинг автоматически.

```go
// internal/common/decorator/decorator.go

// ApplyCommandDecorators wraps handler with tracing (outermost, so log
// records and metrics observe the span context), logging and metrics
// (innermost, measuring pure handler time).
func ApplyCommandDecorators[C any](handler CommandHandler[C], d *Decorators) CommandHandler[C] {
    name := useCaseName[C]()
    return commandTracingDecorator[C]{
        base: commandLoggingDecorator[C]{
            base: commandMetricsDecorator[C]{base: handler, deps: d, name: name},
            deps: d,
            name: name,
        },
        deps: d,
        name: name,
    }
}
```

Порядок обёртки не случаен: трейсинг — снаружи, метрики — внутри. Это означает, что когда metrics-декоратор записывает длительность, span-контекст уже установлен и `trace_id` попадёт в атрибуты метрики. Logging-декоратор находится между ними — он видит span-контекст (через `slog` handler, который читает его из context) и пишет defer на named error:

```go
// internal/common/decorator/decorator.go

func (d commandLoggingDecorator[C]) Handle(ctx context.Context, cmd C) (err error) {
    logger := handlerLogger(d.deps, d.name)
    defer func() {
        if err != nil {
            logger.ErrorContext(ctx, "command handler failed", slog.Any("error", err))
            return
        }
        logger.InfoContext(ctx, "command handler succeeded")
    }()

    return d.base.Handle(ctx, cmd)
}

func handlerLogger(d *Decorators, name string) *slog.Logger {
    return d.logger.With(
        slog.String("context", d.contextName),
        slog.String("handler", name),
    )
}
```

`handlerLogger` добавляет `context` (bounded context: «auction», «billing») и `handler` (имя use case: «PlaceBid», «IssueInvoice») к каждой записи. Результат: в логах всегда известно, кто именно выполнял работу, без единой строчки в хендлере.

Tracing-декоратор открывает span с именем `"commands/" + name` — и именно поэтому в Jaeger трейс читается как каталог `app.Application`: спаны `commands/PlaceBid`, `commands/IssueInvoice`, `queries/AuctionCard`.

```go
// internal/common/decorator/decorator.go

func (d commandTracingDecorator[C]) Handle(ctx context.Context, cmd C) (err error) {
    ctx, span := d.deps.tracer.Start(ctx, "commands/"+d.name)
    defer span.End()

    err = d.base.Handle(ctx, cmd)
    recordSpanResult(span, err)
    return err
}
```

Metrics-декоратор пишет histogram с тремя лейблами: `context`, `handler`, `result` (ok/err). Это граница кардинальности: оба лейбла — конечные множества, их значения определены при инициализации, никаких UUID, user_id или slug ошибки в лейблах.

```go
// internal/common/decorator/decorator.go

func (d commandMetricsDecorator[C]) Handle(ctx context.Context, cmd C) (err error) {
    start := time.Now()
    defer func() {
        d.deps.commandDuration.Record(
            ctx,
            time.Since(start).Seconds(),
            metric.WithAttributes(durationAttributes(d.deps.contextName, d.name, err)...),
        )
    }()

    return d.base.Handle(ctx, cmd)
}

func durationAttributes(contextName, handlerName string, err error) []attribute.KeyValue {
    result := "ok"
    if err != nil {
        result = "err"
    }
    return []attribute.KeyValue{
        attribute.String("context", contextName),
        attribute.String("handler", handlerName),
        attribute.String("result", result),
    }
}
```

Добавление нового use case — новый файл в `app/command/`. Ни одной строчки observability-кода. Дашборд появляется автоматически.

### Домен не логирует. Почему

Доменный код в Molot — stdlib only, без внешних зависимостей. Если бы `auction.Auction.PlaceBid` делал `slog.Info(...)`, домен получил бы зависимость от конкретного логгера. Это нарушает инверсию зависимостей и делает domain-unit-тесты обязанными настраивать logger.

Второй аргумент: домен возвращает ошибки с богатой типизацией (`ErrBidBelowMinimum`, `ErrVerificationRequired`). Это информация, а не лог. Логировать её — работа декоратора: он видит ошибку на выходе `Handle` и пишет запись с нужным уровнем. Домен остаётся чистым.

Исключение задокументировано явно: `ForbiddenInvoiceAccessError` логируется на уровне WARN в декораторе **до** маппинга в 404. Наружу уходит «invoice-not-found» (анти-enumeration), а в логах остаётся аудит-трейл с реальным `actor` и `debtor` — для безопасников.

### Логи: slog с контекстным обогащением

`internal/common/logs/logs.go` собирает logger один раз: JSON в prod (`LOG_FORMAT=json`), text локально. Но главное — `NewContextHandler`, который оборачивает любой `slog.Handler` и при каждой записи дополняет её атрибутами из context:

```go
// internal/common/logs/logs.go

func (h contextHandler) Handle(ctx context.Context, rec slog.Record) error {
    if id, ok := CorrelationIDFromContext(ctx); ok {
        rec.AddAttrs(slog.String("correlation_id", id))
    }
    if spanCtx := trace.SpanContextFromContext(ctx); spanCtx.IsValid() {
        rec.AddAttrs(
            slog.String("trace_id", spanCtx.TraceID().String()),
            slog.String("span_id", spanCtx.SpanID().String()),
        )
    }
    return h.next.Handle(ctx, rec)
}
```

Два источника обогащения: `trace_id`/`span_id` — из OTel span context (установленного tracing-декоратором или otelhttp), `correlation_id` — из Watermill CorrelationID middleware (установленного перед вызовом event handler). Ни один хендлер не заботится об этих атрибутах сам — они приходят через context.

### Трейсы: сквозной путь через шину

Самое интересное в event-driven системе — как трейс «выживает» при асинхронной передаче через Watermill. Ответ: W3C `traceparent` header кладётся в metadata сообщения при публикации и извлекается при обработке.

`internal/common/watermill/watermill.go` реализует `metadataCarrier`, который адаптирует Watermill `message.Metadata` к интерфейсу `propagation.TextMapCarrier`. Middleware `observe` использует его для Extract:

```go
// internal/common/watermill/watermill.go

func observe(h message.HandlerFunc) message.HandlerFunc {
    return func(msg *message.Message) ([]*message.Message, error) {
        ctx := msg.Context()

        if correlationID := middleware.MessageCorrelationID(msg); correlationID != "" {
            ctx = logs.ContextWithCorrelationID(ctx, correlationID)
        }

        ctx = otel.GetTextMapPropagator().Extract(ctx, metadataCarrier(msg.Metadata))

        handlerName := message.HandlerNameFromCtx(ctx)
        if handlerName == "" {
            handlerName = "unknown"
        }

        ctx, span := otel.Tracer(tracerName).Start(ctx, "events/"+handlerName)
        defer span.End()

        msg.SetContext(ctx)

        produced, err := h(msg)
        // ...
        return produced, err
    }
}
```

`otel.GetTextMapPropagator().Extract(ctx, metadataCarrier(msg.Metadata))` восстанавливает span context из metadata и делает открываемый span дочерним к publishing-спану. Один и тот же `traceID` — в HTTP-запросе, в командном хендлере, в outbox publish и в event handler Settlement.

`internal/common/tracing/tracing.go` при инициализации регистрирует W3C composite propagator:

```go
// internal/common/tracing/tracing.go

otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
    propagation.TraceContext{},
    propagation.Baggage{},
))
```

Итоговая карта спанов для операции «ставка» (из ARCHITECTURE.md §11):

| Слой | Span |
|---|---|
| HTTP-вход | `POST /api/auctions/{auctionID}/bids` (otelhttp + spanRouteNamer) |
| Команда | `commands/PlaceBid` (tracing-декоратор) |
| БД | `db.query`, `db.tx` (otelsql) |
| Outbox publish | `publish auction-events` |
| Event handler | `events/OnAuctionClosedStartSettlement` |
| Фасад саги | `facade/auction.AwardToRunnerUp` |
| PSP | `psp.Charge` (атрибут idempotency_key) |

Путь «молоток → счёт → оплата → расчёт» — это один трейс с 67 спанами. В Jaeger видно: Settlement handler стартовал через 12 мс после закрытия аукциона, PSP Charge занял 340 мс, а вся сага от AuctionClosedV1 до ConfirmSettlement — 1.2 с.

Этот факт прямо влияет на операционные решения: если PSP начинает тормозить, это видно на спане `psp.Charge` раньше, чем ухудшится какая-либо RED-метрика команды PayInvoice — потому что трейс показывает конкретный участок цепи.

### Метрики: каждая отвечает на вопрос дежурного

**`molot_command_duration_seconds{context, handler, result}`** — histogram из metrics-декоратора. Rate отвечает: «сколько PlaceBid в секунду обрабатывает billing?» Errors rate: «где именно появились ошибки — в auction или settlement?» p95/p99: «хендлер IssueInvoice стал медленнее после деплоя?»

Grafana-дашборд `molot-contexts-red` строит три панели на каждый тип: rate, error rate, p95/p99. Переменные `context` и `handler` позволяют сузить до одного use case. Это первый экран, который открывает дежурный.

**`molot_bus_dead_letter_size`** — gauge по таблице `watermill_events_dead_letter`. Значение > 0 — всегда инцидент, никогда не «бизнес-шум» (ARCHITECTURE.md §6.8). В дашборде `molot-bus-saga` — stat-панель с красным фоном при любом ненулевом значении. Дежурный смотрит на неё: «Есть ли сообщения, которые не смогли обработаться после 5 ретраев?» Ответ бинарный.

**`molot_bus_oldest_message_age_seconds{topic}`** — возраст старейшего необработанного сообщения в outbox по топику. Растёт — консьюмер не успевает. Стоит — норма. Дежурный: «Шина справляется с текущей нагрузкой?»

**`molot_bus_retries_total{handler}`** — счётчик из middleware `retryCounter`, который сидит между Retry и Recoverer в цепочке:

```go
// internal/common/watermill/watermill.go

retryCounter := func(h message.HandlerFunc) message.HandlerFunc {
    return func(msg *message.Message) ([]*message.Message, error) {
        produced, err := h(msg)
        if err != nil {
            handlerName := message.HandlerNameFromCtx(msg.Context())
            if handlerName == "" {
                handlerName = "unknown"
            }
            retriesTotal.Add(msg.Context(), 1, metric.WithAttributes(
                attribute.String("handler", handlerName),
            ))
        }
        return produced, err
    }
}
```

Rate этого счётчика — ранний сигнал: база деградирует, но до dead-letter ещё далеко. Дежурный: «Есть ли систематические ошибки в каком-то хендлере?»

**`molot_saga_transitions_total{from, to, reason}`** — counter из settlement repo-адаптера. В норме большинство transitions — `AwaitingPayment → Settled`. Рост `reason=second_chance_declined` сигнализирует о поведении пользователей. Рост `AwaitingPayment → Relisted` с `reason=payment_timeout` — о проблемах с PSP или UX оплаты. Дежурный: «Какой процент аукционов не завершается оплатой с первой попытки?»

**`molot_settlement_nonterminal_age_seconds`** — максимальный возраст саги, не достигшей терминального состояния (Settled / Relisted / FailedUnsold). Алерт: > 2×PAYMENT_TERM. В дашборде — stat с оранжевым при 1×PAYMENT_TERM и красным при 2×. Дежурный: «Есть ли зависшие саги?» Если метрика растёт монотонно — сага застряла, событие потеряно или лежит в dead-letter. Первый шаг: открыть `molot-bus-saga`, проверить dead-letter.

**`molot_worker_due_backlog{worker}`** — накопленный backlog воркеров закрытия (ClosingWorker) и экспирации (ExpiryWorker). Рост означает, что воркер не успевает обработать все due-аукционы/инвойсы за тик. Дежурный: «Не накапливаются ли просроченные закрытия, которые блокируют старт саги?»

### Pipeline: от приложения до Grafana

`internal/common/metrics/metrics.go` и `internal/common/tracing/tracing.go` инициализируют OTLP gRPC экспортеры на один endpoint (local collector). Оба пакета — независимы: сделано намеренно (комментарий в `newResource()`), по той же логике, по которой `Money` дублируется в auction и billing.

`deploy/otel-collector.yaml` описывает pipeline:

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

processors:
  batch:

exporters:
  otlp/jaeger:
    endpoint: jaeger:4317
    tls:
      insecure: true
  prometheus:
    endpoint: 0.0.0.0:8889

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/jaeger]
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [prometheus]
```

Схема: приложение → OTLP gRPC (port 4317) → collector → трейсы в Jaeger (OTLP), метрики в Prometheus scrape endpoint (8889) → Grafana.

Go runtime instrumentation (GC pauses, goroutines, heap) стартует в `NewMeterProvider` через `runtime.Start(runtime.WithMeterProvider(provider))` — базовая гигиена без дополнительного кода.

### Readiness: гейтирование на реальном состоянии

`/healthz` — liveness, всегда 200. `/readyz` — readiness с двумя пробами: ping DB и `router.Running()`. Второе — критично: Watermill router поднимается асинхронно после HTTP-сервера. До `router.Running()` event handler-ы не обрабатывают сообщения — принимать трафик, который генерирует события, бессмысленно.

```go
// internal/common/server/server.go

r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
    for _, probe := range readiness {
        if err := probe.Check(req.Context()); err != nil {
            logger.WarnContext(req.Context(), "readiness probe failed",
                slog.String("probe", probe.Name), slog.Any("error", err))
            http.Error(w, probe.Name+" not ready", http.StatusServiceUnavailable)
            return
        }
    }
    w.WriteHeader(http.StatusOK)
})
```

`RegisterHealthEndpoints` принимает slice `ReadinessProbe` — именованные функции. Composition root в `cmd/monolith/main.go` передаёт две: `postgres.Ping` и `router.Running`. Если добавится Redis — просто добавляется третья проба, без изменения сервера.

---

## Трейдоффы

**Зависимость от OTel SDK везде.** `internal/common` импортирует `go.opentelemetry.io/otel`. Это осознанный выбор: OTel — стандарт отрасли, SDK стабилен, альтернативы (самописный tracer) хуже по всем осям. Трейдофф принят в ADR-0005.

**Вес docker-compose.** Четыре дополнительных контейнера (otel-collector, Jaeger, Prometheus, Grafana) утяжеляют локальный стенд. Цена принята для reference-системы: наблюдаемость нельзя добавить «потом».

**Дисциплина кардинальности меток.** `context` и `handler` — конечные множества, определённые при инициализации. Нарушение (добавить `auction_id` в метку) взорвёт cardinality Prometheus. Это организационный риск, не технический: код не защищает от него автоматически.

**Отсутствие sampling.** OTLP-экспортер в `tracing.go` использует default `AlwaysSample`. Для production с высоким RPS нужен `TraceIDRatioBased` sampler или head-based sampling в collector. В Molot это не настроено — reference-система работает на низком RPS.

---

## Типичные ошибки

**Логи `fmt.Println` «для скорости».** Нет форматирования, нет уровня, нет `trace_id`. В production это означает: поиск по тексту руками, невозможность join с трейсом, случайный порядок concurrent записей. Цена замены на `slog.InfoContext` — одна строка.

**Метрики без лейблов** или, наоборот, **с бесконечными лейблами**. Без лейблов `context`/`handler` дашборд показывает суммарный rate всего приложения — бесполезно для диагностики. С лейблом `auction_id` — Prometheus убивает память через час нагрузочного теста. Правило: лейбл допустим только если его мощность измеряется десятками, а не тысячами.

**Span в каждом хендлере вручную.** Три строки копипаста: `tracer.Start`, `defer span.End()`, `recordSpanResult`. Умножить на 15 хендлеров — 45 мест, где можно забыть `defer`, записать неверный статус или пропустить ошибку. Декоратор делает это один раз, корректно.

**Алерты на CPU/memory вместо симптомов.** CPU растёт при нагрузке — это нормально. Dead-letter > 0 при любой нагрузке — это всегда инцидент. Сага со временем жизни > 2×PAYMENT_TERM — проблема, не зависящая от CPU. Alerting на RED-метрики и на бизнес-индикаторы (dead-letter, saga age) даёт сигнал до того, как пользователи начали жаловаться.

**`router.Running()` не в readiness probe.** Если readyz смотрит только на ping DB, Kubernetes начнёт направлять трафик к поду до того, как Watermill готов. Запросы придут, команды выполнятся, события лягут в outbox — и останутся там необработанными до старта router. Временная рассинхронизация, которую сложно отследить без инструментации шины.

---

## Чек-лист

- [ ] Все command/query хендлеры обёрнуты `ApplyCommandDecorators` / `ApplyQueryDecorators` — проверить `service/service.go` каждого контекста.
- [ ] `NewContextHandler` используется в `NewLogger` — `trace_id` и `correlation_id` присутствуют в каждой prod-записи.
- [ ] Watermill router использует `observe` middleware — `correlation_id` доступен в event handler-ах.
- [ ] Publisher-обёртка кладёт W3C `traceparent` в metadata сообщения (inject перед публикацией).
- [ ] `W3C TraceContext + Baggage` зарегистрированы в `otel.SetTextMapPropagator` при инициализации.
- [ ] Лейблы метрик — только конечные множества (`context`, `handler`, `result`). Никаких ID в лейблах.
- [ ] Алерт на `molot_bus_dead_letter_size > 0` настроен и проверен.
- [ ] Алерт на `molot_settlement_nonterminal_age_seconds > 2×PAYMENT_TERM` настроен.
- [ ] `/readyz` включает `router.Running()` — не только ping DB.
- [ ] Домен не содержит ни одного import логгера или метрик.
- [ ] `ForbiddenInvoiceAccessError` логируется на WARN до маппинга в 404 — audit trail сохраняется.
- [ ] `AlwaysSample` заменён на ratio-based sampler перед выходом в production с высоким RPS.

---

## Ссылки и источники

- **ARCHITECTURE.md §11** — полная карта спанов: слой, имя спана, кто создаёт; таблица метрик с назначением и алертами; readiness endpoint.
- **ADR-0005** (`docs/adr/0005-observability.md`) — решение, трейдоффы, отклонённые альтернативы (только логи, logrus в хендлерах, метрики без трейсинга).
- **BOOK_AUDIT.md правила 47–50** — требования к observability в контракте ревью: правило 47 (cross-cutting в одном месте), 48 (домен без логов), 49 (RED per use case), 50 (операционка: runbook, rollback).
- `internal/common/decorator/decorator.go` — `ApplyCommandDecorators`, `ApplyQueryDecorators`, `commandMetricsDecorator`, `commandTracingDecorator`, `commandLoggingDecorator`.
- `internal/common/logs/logs.go` — `NewLogger`, `NewContextHandler`, `ContextWithCorrelationID`.
- `internal/common/watermill/watermill.go` — `NewRouter`, `observe` middleware, `metadataCarrier`, `retryCounter`.
- `internal/common/tracing/tracing.go` — `NewTracerProvider`, W3C propagator.
- `internal/common/metrics/metrics.go` — `NewMeterProvider`, runtime instrumentation.
- `internal/common/server/server.go` — `RegisterHealthEndpoints`, `ReadinessProbe`, `spanRouteNamer`.
- `deploy/otel-collector.yaml` — pipeline app → collector → Jaeger / Prometheus.
- `deploy/grafana/dashboards/molot-contexts-red.json` — RED-дашборд команд/запросов: rate, error rate, p95/p99 per handler.
- `deploy/grafana/dashboards/molot-bus-saga.json` — дашборд шины и саги: dead-letter (stat + time series), oldest message age, retries by handler, saga transitions, nonterminal age, worker backlog.
