# Operations — runbook Molot

> Правило 50: без задокументированных rollback/revert/undo-миграций тест-сьют не считается достаточным.
> Алертная политика и метрики — `ARCHITECTURE.md` §11; dead-letter — §6.8.

## 1. Rollback бинаря

Деплой — образ из `build/Dockerfile`, состояние живёт только в Postgres, бинарь stateless.

1. Откатить тег образа на предыдущий: `docker compose pull app && docker compose up -d app`
   (или пере-pin тега в манифесте деплоя на прошлый digest).
2. Несколько реплик безопасны: корректность конкурентных воркеров держат row lock + guard-ы
   агрегатов + `version` (§10), даунгрейд можно катить поэтапно.
3. Условие совместимости: предыдущий бинарь должен уметь читать текущую схему БД.
   Миграции пишутся backward-compatible на одну версию (expand → migrate → contract);
   если откат пересекает contract-миграцию — сперва `goose down` (см. §3).

## 2. Revert кода

- `git revert <sha>` — единственный способ отката изменения в main (история не переписывается).
- Revert коммита с миграцией НЕ удаляет применённую миграцию из БД: сначала откат схемы
  (`goose down`), затем revert кода, затем обычный деплой.

## 3. Undo миграций (goose, embedded per-context)

Миграции живут в `internal/<ctx>/adapters/migrations/` и применяются на старте бинаря.
Каждая миграция обязана иметь рабочую секцию `-- +goose Down`.

Откат последней миграции конкретного контекста (goose CLI поверх той же директории):

```sh
goose -dir internal/auction/adapters/migrations postgres "$DATABASE_URL" down
goose -dir internal/billing/adapters/migrations postgres "$DATABASE_URL" down
goose -dir internal/participant/adapters/migrations postgres "$DATABASE_URL" down
goose -dir internal/settlement/adapters/migrations postgres "$DATABASE_URL" down
goose -dir internal/notification/adapters/migrations postgres "$DATABASE_URL" down
```

Статус: `goose -dir ... postgres "$DATABASE_URL" status`. Контексты независимы
(схема-на-контекст, у каждого своя goose-таблица версий) — откатывается только
затронутый контекст. После `down` деплоится бинарь, не требующий откатанной схемы.

## 4. Dead-letter redelivery (§6.8)

`molot_bus_dead_letter_size > 0` — всегда инцидент, не бизнес-шум. Алерт срабатывает немедленно
(панель «Dead letter size» в дашборде «Molot — Bus & Saga»).

**Что попадает** (после 5 ретраев exp backoff, ~1 мин бюджета):
(а) poison message — битый payload / unmarshal-ошибка;
(б) инфраструктурная деградация дольше ретрай-бюджета (БД недоступна, паника фасада);
(в) программный баг хендлера.
**Не попадает by design:** бизнес-дубликаты (ack-аются guard-ами), declined-платежи
(синхронный путь), «нет такого аукциона» в проекциях (upsert).

Порядок действий:

1. **Прочитать** очередь: `SELECT "offset", uuid, payload, metadata, created_at FROM watermill_events_dead_letter ORDER BY "offset";`
   В `metadata` — исходный топик, имя хендлера и `correlation_id` → трейс в Jaeger (UI: :16686).
2. **Классифицировать** причину по payload/metadata/трейсу: (а), (б) или (в).
3. **Для (б)/(в)** — устранить причину (поднять БД / выкатить фикс), затем re-publish
   сообщений в исходный топик с сохранением metadata (uuid, correlation_id).
   Это безопасно: каждый хендлер идемпотентен (§6.6), повторная доставка — штатный режим.
4. **Для (а)** — починить producer/схему события; payload мигрировать вручную
   (поправить и re-publish) либо задокументированно отбросить.
5. **Подтвердить дренаж**: `molot_bus_dead_letter_size == 0`, lag
   (`molot_bus_oldest_message_age_seconds`) вернулся к норме.

## 5. Сопутствующие сигналы

| Симптом | Куда смотреть |
|---|---|
| Растёт `molot_bus_oldest_message_age_seconds` | подписчики не успевают / router упал — `/readyz` (гейтится на `router.Running()`), логи по `correlation_id` |
| `molot_settlement_nonterminal_age_seconds` > 2×PAYMENT_TERM | зависшая сага: `GET /api/auctions/{id}/settlement` (роль operations), state + failure_reason; искать шов в §6.7 |
| Растёт `molot_worker_due_backlog` | воркеры закрытия/экспирации не успевают: tick duration, насыщение пула (sql.DBStats) |
| `molot_psp_calls_total{result="unavailable"}` | деградация PSP; клиентские ретраи безопасны (idempotency key = invoiceID, §3.1) |
