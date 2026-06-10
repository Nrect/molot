# ADR-0002: Postgres как шина событий (Watermill + watermill-sql, transactional outbox)

Статус: принят (2026-06-11). Связан: ARCHITECTURE.md §4.4, §6.8, §7 (watermill-таблицы); BOOK_AUDIT.md §6.

## Контекст

Контексты интегрируются асинхронными версионированными событиями (`internal/<ctx>/events/`, V1).
Нужны: durable публикация атомарно с изменением агрегата (at-least-once, ничего не теряется на краше),
ретраи, dead-letter, упорядоченность по агрегату. Kafka на MVP-масштабе (сотни msg/s максимум) — лишняя
операционная нагрузка; Postgres уже есть и держит состояние всех агрегатов.

## Решение

Watermill + watermill-sql v4 поверх той же Postgres 16:

- **Transactional outbox**: `sql.Publisher` привязывается к той же `*sql.Tx`, что и persist агрегата;
  событие и состояние коммитятся атомарно. Маппинг domain → integration — в `adapters/events_mapper.go`.
- `DefaultPostgreSQLSchema` + `DefaultPostgreSQLOffsetsAdapter`, `InitializeSchema: true`; таблица на топик
  (`watermill_auction_events`, `watermill_billing_events`, `watermill_participant_events`) + offsets per consumer group.
- **Один `message.Router` на бинарь**; middleware строго: `CorrelationID → PoisonQueue(events.dead_letter) →
  Retry{MaxRetries: 5, exp backoff ≤30s} → Recoverer`. PoisonQueue снаружи Retry: SQL-подписчик упорядочен,
  одно вечно-падающее сообщение остановило бы поток — после исчерпания ретраев оно уходит в dead-letter
  (алерт > 0 немедленно, runbook — ARCHITECTURE.md §6.8).
- Типизированные хендлеры через `cqrs.NewEventProcessorWithConfig` + `cqrs.NewEventHandler`,
  `cqrs.JSONMarshaler{GenerateName: cqrs.StructName}`; регистрация — `svc.RegisterEventHandlers(processor)`.
- Readiness гейтится на `router.Running()`; `router.Run(ctx)` в одном errgroup с HTTP и воркерами.
- At-least-once ⇒ каждый хендлер идемпотентен по натуральному ключу (таблица — ARCHITECTURE.md §6.6),
  покрыто тестом повторной доставки.

## Последствия

- (+) Ноль дополнительной инфраструктуры; атомарность публикации тривиально корректна; локальная разработка =
  прод по код-путям.
- (+) Миграция на Kafka = замена publisher/subscriber (+ Watermill forwarder как мост); контракты `events/`,
  хендлеры и тесты не трогаются.
- (−) Пропускная способность ограничена (polling, сотни msg/s) и есть задержка доставки — для reference-системы
  достаточно; узкое место заранее обложено метриками (`molot_bus_oldest_message_age_seconds`, lag offsets,
  retries) — сигнал к миграции виден до боли.
- (−) Шина живёт в той же БД: бэкапы/нагрузка общие; учитывать при capacity planning.

## Альтернативы

- **Kafka с первого дня** — отклонено: ops-стоимость без нужды; дизайн и так оставляет замену дешёвой.
- **In-process диспетчеризация (goroutines/channels)** — отклонено: нет durability, события теряются на краше,
  невозможен outbox.
- **Postgres LISTEN/NOTIFY** — отклонено: нет durable offsets, at-most-once семантика.
