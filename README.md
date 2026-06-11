# Molot — эталонная бизнес-система на Go

Аукционная площадка (английский аукцион), построенная как **эталон долгоживущей бизнес-системы**: модульный монолит, DDD + CQRS + Clean Architecture, событийная интеграция через transactional outbox, сага с компенсациями, сквозная observability. Архитектура выведена из книги «Go With The Domain» (Three Dots Labs) и зафиксирована контрактом из 55 правил.

> Проект учебно-боевой: каждое решение здесь — образец для переноса в реальные системы с большими бизнес-требованиями. Читается сверху вниз: документы → домен → всё остальное.

## Как читать этот репозиторий

1. **[docs/BOOK_AUDIT.md](docs/BOOK_AUDIT.md)** — контракт: 55 императивных правил архитектуры (слои, DDD-тактика, repository, CQRS, события, тесты, observability). Всё ревьюится против него.
2. **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — рабочая спека системы: context map, агрегаты и инварианты, каталог команд/запросов, события, сага (таблица переходов, идемпотентность, crash-seam карта), схема БД, HTTP API, воркеры, observability-карта.
3. **[docs/event-storming.md](docs/event-storming.md)** — доменный поток и маппинг «событие/команда → Go-тип/хендлер».
4. **[docs/adr/](docs/adr/)** — решения с трейдоффами: модульный монолит, Postgres-шина (watermill-sql), граница агрегата Auction, сага settlement, observability.
5. **Код домена** — начни с [internal/auction/domain/auction](internal/auction/domain/auction) и [internal/settlement/domain/settlement](internal/settlement/domain/settlement) (state machine саги).
6. **[docs/REVIEW.md](docs/REVIEW.md)** — отчёт ревью реализации против контракта; [docs/test-taxonomy.md](docs/test-taxonomy.md) — уровни тестов; [docs/operations.md](docs/operations.md) — runbook (rollback, dead-letter redelivery).

## Домен в двух словах

Продавец выставляет лот → участники ставят (шаг ставки, анти-снайпинг продлевает окно, резервная цена скрыта) → закрытие по времени → победителю счёт (цена + комиссия) → оплата через PSP → не оплатил в срок: оферта second-chance второму бидеру ИЛИ перевыставление лота (cap = 1) → расчёт завершён/провален. Каждый шаг после молотка оркестрирует сага settlement с идемпотентными компенсациями.

## Bounded contexts

| Контекст | Суть | Самое интересное |
|---|---|---|
| `auction` | Лоты, ставки, закрытие, витрины | Агрегат с топ-2 ставками денормализованно + append-only история в границе согласованности; optimistic locking; анти-снайпинг; гонка PlaceBid vs Close решена row lock + guard |
| `billing` | Счета, PSP, таймаут оплаты | Charge до транзакции + идемпотентный Refund (charged-but-expired гонка закрыта); guard-таблица переходов Invoice; анти-enumeration (чужой счёт = 404) |
| `settlement` | Сага расчётов | Протокол decide → effect → commit; fast-forward на опережающие события; crash-seam тест на каждый шов |
| `participant` | Регистрация, верификация | Маленький контекст, который НЕ over-engineered |
| `notification` | События → email | Тривиальный консьюмер без слоёв — задокументированное исключение |

Связь контекстов: события через **transactional outbox** (watermill-sql: Postgres как шина; Kafka — заменой publisher/subscriber, ADR-0002) + синхронные фасады для команд саги (consumer-side интерфейсы, ADR-0004).

## Запуск

```bash
cp .env.example .env          # дефолты годятся для локалки
docker compose up -d          # postgres + app + otel-collector + jaeger + prometheus + grafana
# или локальный бинарь против compose-постгреса:
make run
```

| Сервис | URL |
|---|---|
| API | http://localhost:8080 (healthz/readyz; всё бизнесовое под `/api`, JWT HS256) |
| Jaeger (трейсы) | http://localhost:16686 — путь «молоток → счёт → оплата → расчёт» виден одним трейсом |
| Prometheus | http://localhost:9090 |
| Grafana (дашборды) | http://localhost:3000 — «Molot — Contexts RED», «Molot — Bus & Saga» |

## Тесты

```bash
make test               # unit (domain + app), -race, без докера
make test-integration   # адаптеры против реального Postgres
make test-component     # приложение целиком in-process, фейковый PSP
make test-e2e           # прод-бинари из docker compose, публичный HTTP
```

Таксономия уровней — [docs/test-taxonomy.md](docs/test-taxonomy.md). Обязательные паттерны: rollback-тест каждого транзакционного repo, race-тесты («20 горутин, ровно один победитель»), идемпотентность каждого event-хендлера повторной доставкой, crash-seam continuation саги.

## Раскладка

```
cmd/monolith/            тонкий main: процесс, сигналы, HTTP-листенер
internal/monolith/       composition root (прод + компонентные тесты → общий newApplication)
internal/common/         инфраструктура: CQRS-декораторы (логи+RED+спаны), errs, auth,
                         postgres (RunInTx/FinishTransaction), watermill (router, outbox, метрики шины)
internal/<context>/
  domain/<aggregate>/    stdlib-only: инварианты, behavior-методы, sentinel-ошибки, Repository-интерфейс
  app/{command,query}/   use cases; consumer-side интерфейсы зависимостей
  ports/                 HTTP (oapi-codegen strict), event-хендлеры, воркеры
  adapters/              Postgres/in-memory repo, events_mapper (outbox в той же tx), миграции goose
  events/                плоские версионированные V1-контракты — единственный импортируемый снаружи пакет
  service/               сборка контекста + фасад для саги
api/openapi/             контракты (codegen через make openapi)
tests/                   component + e2e
```

Направление зависимостей enforced: domain ← app ← ports/adapters; чужой `domain/` не импортируется; common без бизнес-типов.

## Что дальше (план развития)

- Kafka вместо watermill-sql — замена publisher/subscriber + forwarder, контракты не меняются (ADR-0002).
- Вынос контекста в сервис — события уже версионированы, фасад заменяется на gRPC-адаптер.
- Read store (Elastic и т.п.) — за существующими read-model интерфейсами.
- Алерты поверх готовых метрик: `molot_bus_dead_letter_size > 0`, возраст незавершённых саг.
