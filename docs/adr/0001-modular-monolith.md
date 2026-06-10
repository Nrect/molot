# ADR-0001: Модульный монолит, а не микросервисы

Статус: принят (2026-06-11). Связан: ARCHITECTURE.md §1, §9; BOOK_AUDIT.md §2, правила 1–7, 23.

## Контекст

Molot — эталонная аукционная площадка: 5 bounded contexts (auction, participant, billing, settlement,
notification), инкремент MVP-масштаба (~месяц, один разработчик). По Accelerate/DORA производительность
даёт loose coupling, а не сетевые границы; неправильно нарезанные микросервисы = distributed monolith
(та же связность + сеть + тулинг). Границы контекстов получены из event-storming
(`docs/event-storming.md`), а не из «похожих сущностей».

## Решение

Один бинарь `cmd/monolith/main.go` (единственный composition root), один `go.mod`, без `pkg/`.
Первое деление репозитория — по bounded context: `internal/<ctx>/{domain,app,ports,adapters,events,service}`;
слои — второе, внутри контекста.

Границы enforce-ятся компилятором и CI, не дисциплиной на словах:

- `internal/` — compiler-enforced privacy;
- `go-cleanarch` в CI: domain → ничего, app → domain, ports/adapters → внутрь, ports не импортирует adapters;
- импорт чужого `domain/` и запросы к чужим Postgres-схемам — fail CI/ревью; снаружи контекста импортируются
  только `events/` и фасад `service/` через consumer-defined интерфейс;
- `internal/common` — только инфраструктура, ноль бизнес-типов (`Money` продублирован в auction и billing осознанно).

Исключение по правилу 33: notification — без domain/app-слоёв (тривиальный event→template→send);
задокументировано, пересматривается при росте логики.

## Последствия

- (+) Один деплой, один docker-compose, транзакционный outbox на общей Postgres, in-process фасады саги —
  дёшево и трассируемо.
- (+) Путь выноса заложен дизайном, не кодом «на будущее»: контекст → сервис = замена facade-адаптера на gRPC
  и watermill-sql на Kafka (ADR-0002); контракты `events/` и хендлеры не меняются.
- (−) Масштабирование — только репликами целого бинаря; воркеры и event-хендлеры спроектированы под
  конкурентные реплики (row lock + guard-ы агрегатов, ARCHITECTURE.md §10).
- (−) Соблазн «срезать» границу внутри одного процесса постоянен — поэтому guard-ы в CI, а решения,
  тянущие чужой домен (правила second-chance), перенесены в данные событий (`RunnerUpQualifies`) и домен саги.

## Альтернативы

- **Микросервисы с первого дня** — отклонено: операционная цена без организационной нужды, высокий риск
  distributed monolith при ещё не проверенных границах.
- **Слоистый монолит без контекстов** (top-level `internal/handlers`, `internal/repositories`) — отклонено:
  связность растёт неконтролируемо, выноса нет, границы пришлось бы вводить задним числом.
