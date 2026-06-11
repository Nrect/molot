# Таксономия тестов (правило 39)

> Каждый тест в репо классифицируем в ровно одну строку таблицы. Имена уровней едины в пакетах, Makefile и CI. Терминологические споры решает таблица, не вкус.

| Уровень | Docker DB | Внешние системы | Бизнес-фокус | Моки | Тестируемый API | Build tag | Команда |
|---|---|---|---|---|---|---|---|
| **Unit (domain)** | нет | нет | да | нет (ноль) | Go package (black-box `_test`) | — | `make test` |
| **Unit (app)** | нет | нет | оркестрация | recording spies | Go package | — | `make test` |
| **Integration** | да | нет | нет | обычно нет | Go package (адаптеры) | `integration` | `make test-integration` |
| **Component** | да | нет | да | только внешние (PSP fake) | HTTP | `component` | `make test-component` |
| **E2E** | да | да (нет внешних в Molot) | да | нет | HTTP (только публичный) | `e2e` | `make test-e2e` |

## Правила уровней

**Unit (domain)** — corner cases агрегатов: black-box (`package auction_test`), table-driven, snake_case-имена кейсов, фикстуры только через доменный API, `go-cmp` + `cmp.AllowUnexported`, sentinel-ошибки ассертятся `errors.Is`. Тесты-зеркала чистых функций не пишем.

> **Deliberate deviation from rule 40 (go-cmp + AllowUnexported).** All domain value objects in this project are single-field comparable wrappers (e.g. `AuctionID`, `InvoiceID`, `Money`), so testify's deep-equality assertions (`assert.Equal`, `assert.ErrorIs`) are sufficient and clearer than `go-cmp` with an `AllowUnexported` option list. If a multi-field, non-comparable struct is ever added to the domain, switch those comparisons to `go-cmp + cmp.AllowUnexported` as rule 40 requires.

**Unit (app)** — только реальная оркестрация (порядок вызовов, прокидывание значений, маппинг sentinel → no-op). Моки — рукописные recording spies. «Метод был вызван» сам по себе — не ассерт. Бизнес-сценарий в app-тесте = логика утекла из домена → перенести.

**Integration** — «правильно ли МЫ используем Postgres»: один shared black-box suite на pg+inmem реализации каждого repo; `t.Parallel()` везде; изоляция уникальными данными (uuid), cleanup запрещён; ассерты по конкретному ID, не по длине коллекции; sleep/retries запрещены (`assert.Eventually` — крайний случай для проекций). Обязательные: rollback-тест (updateFn мутирует и возвращает ошибку → старое состояние живо) и race-тесты (`close(start)`, ровно один победитель). Гейт: пропускаются без `TEST_DATABASE_URL`.

**Component** — один процесс приложения целиком, in-process composition root для тестов (`NewComponentTestApplication` → общий `newApplication`), реальные ports + реальная Docker-БД, фейковый PSP. Только happy path — corner cases покрыты ниже. Auth — через реальный JWT-путь (`FakeBidderJWT`).

**E2E** — несколько коротких critical-path флоу на прод-бинарях из docker-compose, только публичный HTTP. Проверяют wiring/контракты, не логику. Частые падения e2e = кто-то сломал контракт.

## Бюджет

Полный локальный юнит-прогон (`make test`) — < 1 мин (цель < 10 с). Все уровни — merge-гейт CI. `-race` — везде, локально и в CI одинаково.

## Дисциплина

- `require.*` — для error/nil-гейтов; `assert.*` — для значений; сообщения у неочевидных ассертов.
- Повторяющиеся мультишаговые проверки → хелперы с `t.Helper()`, верификация round-trip-ом через публичный интерфейс.
- Test sabotage: для non-TDD тестов сложного поведения — сломай реализацию и убедись, что suite падает.
- Тесты, живущие вне CI, гниют гарантированно.
