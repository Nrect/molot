# ADR-0005: Observability — OTel + slog в декораторах и middleware

Статус: принят (2026-06-11). Связан: ARCHITECTURE.md §11; BOOK_AUDIT.md §8, правила 47–50.

## Контекст

«Сервис не протестирован, пока не сломан в проде»: тесты необходимы, но недостаточны — нужны быстрая
диагностика и rollback. Поток «молоток → счёт → оплата → расчёт» пересекает четыре контекста через шину;
без сквозного трейса отладка саги превращается в археологию по логам. Книга использует logrus и самодельные
лог-хелперы; по правилу «modern tooling wins» берём slog + OpenTelemetry, сохраняя книжную позицию:
cross-cutting живёт в одном месте, не размазан по хендлерам.

## Решение

- **Трейсы (OTel)**: otelhttp на входе → tracing-декоратор на каждом command/query (`commands/PlaceBid` —
  трейс читается как каталог `app.Application`) → otelsql на БД → publisher-обёртка кладёт trace/correlation
  в metadata сообщения → Watermill-инструментация открывает дочерний спан на event-хендлере → спаны
  fasade-вызовов саги и PSP (`psp.Charge` с атрибутом idempotency_key). Полная карта спанов — §11.
- **Логи (slog)**: JSON в проде, text локально; обязательные атрибуты — `trace_id`/`span_id` (otel-handler),
  `correlation_id` (Watermill middleware), `context`, `handler`. Logging-декоратор пишет каждый Handle
  (defer на named err) — аудит-трейл независимо от порта. Домен не логирует вообще.
  `ForbiddenInvoiceAccessError` логируется WARN до маппинга в 404 — аудит без утечки наружу.
- **Метрики (OTel/Prometheus)**: RED per use case из metrics-декоратора (`molot_command_duration_seconds
  {context,handler,result}`); шина — `molot_bus_dead_letter_size` (**алерт > 0 немедленно**),
  возраст старейшего сообщения, retries; воркеры — tick duration + due backlog (алерт по порогу);
  сага — `molot_saga_transitions_total{from,to,reason}` + `molot_settlement_nonterminal_age_seconds`
  (алерт > 2×PAYMENT_TERM — «зависшие» саги); PSP calls; пул БД; Go runtime.
- **Health**: `/healthz` liveness; `/readyz` = ping DB + `router.Running()`.
- **Операционка** (правило 50, docs/operations.md): rollback бинаря, `git revert`, undo-миграции goose
  per-context, dead-letter redelivery runbook (§6.8) — пара к тест-сьюту, не замена.

## Последствия

- (+) Один трейс показывает путь события через все контексты; RED-дашборд на каждый контекст возникает
  автоматически — добавление use case не требует ни одной строчки observability-кода.
- (+) Алертами закрыты все «тихие» деградации дизайна: dead-letter, lag шины, backlog воркеров, зависшие саги.
- (−) Дисциплина кардинальности меток (handler/context — конечные множества, никаких ID в метках).
- (−) docker-compose тяжелеет: otel-collector, jaeger, prometheus, grafana — цена принята для reference-системы.

## Альтернативы

- **Только структурные логи** — отклонено: нет причинности через шину, отладка саги по correlation_id
  руками — медленно и ненадёжно.
- **Самодельные лог-хелперы в каждом хендлере (книжный logrus-стиль)** — отклонено: дрейф и дублирование;
  тот же архитектурный замысел реализован generic-декораторами.
- **Метрики без трейсинга** — отклонено: RED показывает «что сломалось», но не «где в цепочке» —
  для event-driven системы недостаточно.
