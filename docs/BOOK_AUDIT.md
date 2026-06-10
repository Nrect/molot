# Аудит: принципы эталонной Go-архитектуры

> Источник: «Go With The Domain» (Three Dots Labs, главы 1–16, проект Wild Workouts) + экспертные знания по модульным монолитам и observability. Конфликты разрешены так: **книга побеждает по архитектуре**, **современный тулинг побеждает по инструментам** (slog вместо logrus, OTel вместо самодельных логов-метрик, `errors`/`%w` стандартной библиотеки допускается наравне с `pkg/errors`-стилем wrap-с-контекстом). Документ — контракт: реализация ревьюится против чек-листа из раздела 10.

---

## 1. Сводная таблица принципов

| Принцип | Почему | Как применяем |
|---|---|---|
| Нет trade-off «качество vs скорость» | Срезание углов экономит только краткосрочно; команда начинает бояться менять код (Accelerate/DORA: производительность даёт loose coupling, а не микросервисы) | DDD Lite + Clean Architecture + CQRS как базовый каркас каждого модуля с реальной логикой |
| Код говорит языком бизнеса (Ubiquitous Language) | Стейкхолдер говорит «schedule training», а не «set state» — иначе двойной перевод и неверно решённые задачи | Методы `ScheduleTraining()`, не `SetState(TrainingScheduled)`; имена пакетов/типов/событий — из словаря домена |
| Типы с поведением, не data bags | Guard + переход состояния в одном методе убивает «стену if-ов» в хендлерах, даёт unit-тесты без моков | Entity с приватными полями, валидирующим конструктором, behavior-методами |
| Always-valid state in memory | Невалидный объект непредставим → исчезают целые классы багов, валидация в одном месте (DRY) | Приватные поля + `NewX(...) (*X, error)` как единственный способ создать экземпляр |
| Домен database-agnostic | Схема хранения ≠ форма домена; в Go без «магии» БД протекает в типы особенно агрессивно | Никаких `db:`/`json:`-тегов на доменных типах; транспортные структуры в адаптере; `context.Context` — допустим |
| Repository: интерфейс в домене, реализация в adapters | Swap БД = признак правильного паттерна; defer решения о хранилище (Domain-First) | `package training: type Repository interface{...}`; in-memory реализация обязательна |
| Мутации через updateFn-замыкание | Транзакция/локинг живут в адаптере, логика — в домене; не таскать tx через context/middleware | `UpdateX(ctx, id, user, updateFn func(ctx, *X) (*X, error)) error` |
| Repository «глупый» | Вся логика в домене → реализации взаимозаменяемы, один shared test suite на все | Repo только load → map → guard → persist; ноль валидации |
| Secure by design | Забытый `if isAdmin` = privilege escalation (кейс Harbor); дизайн должен делать misuse невозможным | Acting user — явный типизированный параметр методов repo; правило в домене (`CanUserSeeTraining`), enforcement в repo |
| CQRS: два модельных пути | Чтения кэшируемы, команды просты, параллельная работа над фичами, read store можно вынести позже | `app.Application{Commands, Queries}`; command = struct + `Handle(ctx, cmd) error`; query = read model interface |
| Слои с направлением зависимостей внутрь | Оптимизация SQL не должна рисковать форматом ответа API; DIP через consumer-side интерфейсы | `ports/app/adapters/domain`; линт `go-cleanarch` в CI |
| Своя модель на каждый слой; DRY — про поведение, не про данные | Общий struct для DB+API = инцидент `LastIp` (случайная утечка поля); потребители меняются по разным причинам | Storage model / domain type / transport DTO раздельно, явный маппинг в адаптере |
| Bounded contexts → модули монолита | Неправильные границы = distributed monolith; правильные — независимые изменения/тесты/деплой | `internal/<context>/{domain,app,ports,adapters,events,service}`; границы из Event Storming, не из «похожих сущностей» |
| Интеграция контекстов — события + outbox | Sync-везде = медленная нестабильная система; at-least-once требует durable публикации | Watermill + watermill-sql (Postgres как шина), версионированные `events/` контракты, идемпотентные хендлеры |
| Тесты = архитектура | Иначе пайплайн — узкое место (час, флаки); тесты должны проектироваться, как код | 4 уровня (unit/integration/component/e2e) с таблицей-таксономией, `t.Parallel()` везде, `make test -race` |
| Observability обязательна, тесты недостаточны | «Сервис не протестирован, пока не сломан в проде»; нужны быстрые rollback | OTel (traces+metrics), slog (JSON, trace-id в атрибутах), RED-дашборды, dead-letter алерты |
| MVP за ~месяц, без кода «на будущее» | «Сделать тяжело добавить потом» = плохой дизайн; модель всё равно неидеальна — оптимизируем изменяемость | Timebox на discovery, ежедневная интеграция рефакторингов, без BDUF |
| Применять пропорционально | Полный стек на тривиальный CRUD = симметричная ошибка (over-engineering) | Простые модули остаются простыми, исключение документируется и пересматривается при росте логики |

---

## 2. Слои и зависимости

### Раскладка репозитория (модульный монолит)

```
cmd/monolith/main.go            # единственный бинарь и единственный composition root
internal/common/                # shared kernel: ТОЛЬКО инфраструктура
internal/<context>/             # по одному пакету на bounded context
    domain/<aggregate>/         # сущности, VO, доменные события, Repository interface (stdlib-only)
    app/command/  app/query/    # use cases (CQRS)
    ports/                      # входящие адаптеры: HTTP-хендлеры, event-хендлеры
    adapters/                   # исходящие: Postgres repo, publishers, клиенты других контекстов
    events/                     # публичные интеграционные события — ЕДИНСТВЕННЫЙ импортируемый снаружи пакет
    service/                    # локальная сборка контекста: NewService(deps) → фасад
go.mod                          # один на репо; pkg/ не создаём — internal/ даёт compiler-enforced privacy
```

Первое деление — **по bounded context**, слои — второе, внутри контекста. Никаких top-level `internal/handlers`, `internal/repositories`.

### Правила направления (CI-enforced, `go-cleanarch`)

- **domain** не импортирует ничего из других слоёв (ни Watermill, ни драйверы, ни transport).
- **app** импортирует только domain.
- **ports** и **adapters** импортируют внутрь; **ports никогда не импортирует adapters**.
- Конкретные типы знает только `main.go` / `service/` (composition root). DI — ручной: конструкторы `NewXxx` принимают интерфейсы и **паникуют на nil-зависимости** («missing trainingRepository») — падать на старте, не на первом вызове. DI-библиотека (wire) — только когда ручная сборка станет болью.

### Consumer-side интерфейсы

Интерфейсы объявляет **потребитель**, рядом с кодом, которому они нужны (паттерн `io.Writer`): app-пакет приватно декларирует `trainingRepository`, `userService`, `trainerService`; адаптеры реализуют их неявно. Это снимает import cycle и даёт маленькие (1–3 метода) интерфейсы, для которых рукописный мок — пара строк. Имена интерфейсов — бизнес-концепты, **никогда** инфраструктура или деплой-топология.

### Shared kernel (`internal/common`)

Только plumbing: логгер, slug-ошибки, auth/user из контекста, generic-декораторы command/query (logging, metrics, tracing), `RunInTx(ctx, db, fn)`, конструкторы Watermill. **Ноль бизнес-типов.** Нужен `Money`/`ProductID` в двух контекстах → дублируем в каждом домене; промоутим в common только провably идентичную и стабильную семантику (редко).

### Связь контекстов

- Запрещено: импорт чужого `domain/`, запросы к чужим таблицам (оба — fail CI).
- По умолчанию: **асинхронные интеграционные события** (через outbox).
- Синхронный вызов (когда процесс синхронен по природе): потребитель определяет интерфейс у себя в `app`, тонкий адаптер в `adapters` зовёт публичный фасад другого контекста; сборка в main. Вынос в gRPC позже = замена одного адаптера.
- Каждый контекст держит **свои проекции** чужих данных (только нужные поля), наполняемые подпиской на события.

### Прочее

- HTTP: без фреймворков — `net/http` + тонкий роутер (chi). Стандартный middleware-стек: RequestID, RealIP, structured logging, Recoverer, CORS, auth, security headers (nosniff, `X-Frame-Options: deny`), NoCache; всё под `/api`.
- Контракты — codegen: OpenAPI (`oapi-codegen`) для публичного HTTP, protobuf для sync RPC; генерация — Makefile-таргет, `.gen.go` руками не правится.
- Конфиг — только env vars с явной ошибкой при отсутствии обязательных; секреты не хардкодятся.
- Всё окружение поднимается одним `docker-compose up` (live reload), эмуляторы/моки переключаются env-переменными; локально и в проде различаются только конфигурацией адаптеров, не код-пути.

---

## 3. DDD-тактика

### Агрегаты и инварианты

- Агрегат = **граница транзакции**; одна транзакция — один агрегат. Агрегаты и bounded contexts **открываются** из доменного потока (Event Storming), а не угадываются по существительным («noun == entity» — guessing, терпимо лишь в простых доменах).
- Все поля entity **приватные**. Единственный способ получить экземпляр — валидирующий конструктор `NewTraining(uuid, userUUID, userName string, t time.Time) (*Training, error)`, отклоняющий каждый пустой/нулевой аргумент. Невалидное состояние непредставимо.
- Behavior-методы названы интентом и применяют переход состояния **атомарно вместе с проверкой инварианта**: `ApproveReschedule(userType)` сам проверяет `IsRescheduleProposed()` и что аппрувер ≠ инициатор, затем `t.time = t.proposedNewTime; t.proposedNewTime = time.Time{}`. Никаких сеттеров, никаких `GetX`-геттеров — только предикаты (`IsAvailable()`, `HasTrainingScheduled()`) и типизированные геттеры для маппинга (`tr.Time()`).
- Receivers: value — для чистых предикатов, pointer — для переходов состояния.
- Sentinel-ошибки — экспортируемые package-level значения: `var ErrHourNotAvailable = errors.New(...)`; возвращаются behavior-методами (`errors.WithStack(ErrX)` для стека), маппятся в транспортные коды только в ports.

### Value objects

- Перечислимые состояния — **отдельные типы**, не строки/булевы пары: `Availability` с константами, конструктор из сырого ввода `NewAvailabilityFromString(s) (Availability, error)`, проверка пустоты через `IsZero()`. «Очистка» — присваивание zero value (`time.Time{}`, `UserType{}`), не указатели-флаги.
- `switch` по закрытому enum обязан `panic(fmt.Sprintf("not supported user type %s", ...))` в `default` — невозможные состояния падают громко.

### Чистота домена

- Домен не импортирует драйверы БД, ORM, transport-пакеты; не несёт сериализационных тегов. Carve-out: `context.Context` в сигнатурах repo — общий Go-концепт, допустим.
- Stateless-вычисления — **простые функции в доменном пакете** (`func CancelBalanceDelta(tr Training, ut UserType) int`), не «service-объекты» («не делай Java в Golang»). Расчёт держится отдельно от мутации (`Cancel()`): SRP + Command-Query Separation.
- Авторизационные правила — чистые доменные функции, возвращающие `error` (nil = можно): `CanUserSeeTraining(user User, t Training) error`.
- Доменная логика — самый стабильный код: переживает смену фреймворков, библиотек, API. Именно поэтому её изолируют.
- Любой бизнес-`if`, замеченный в app-слое, **переезжает в домен** — это формальный признак протечки.
- Появилась «пара простых if-ов» — упрощать сразу: так начинается legacy. Первая модель — расходник; инвестируем в тесты и изоляцию (дёшево менять), не в идеальность.

---

## 4. Repository

### Интерфейс

- Объявлен **в доменном пакете** агрегата, минимальный и generic: `AddTraining / GetTraining / UpdateTraining`. Никаких per-use-case методов (`ApproveTrainingReschedule(updateFn)` на repo — антипаттерн: репозиторий впитывает семантику приложения).
- `ctx context.Context` — первый параметр каждого метода.

### updateFn-паттерн (канонический контракт мутации)

```go
UpdateTraining(ctx context.Context, trainingUUID string, user training.User,
    updateFn func(ctx context.Context, tr *training.Training) (*training.Training, error)) error
```

Реализация: **внутри транзакции** load → unmarshal в доменный тип → auth guard (`CanUserSeeTraining`) → `updateFn` → marshal + persist **возвращённого** значения; ошибка из замыкания = rollback. Возврат значения (а не мутация переданного указателя) — осознанно: явный коммит и гибкость подменить экземпляр. Транзакции через context или middleware — запрещены («магично, неявно, местами медленно»).

### Транзакции и optimistic/pessimistic locking

- SQL: named return + `defer func() { err = m.finishTransaction(err, tx) }()`; rollback при ошибке, при ошибке самого rollback — `multierr.Combine(err, rollbackErr)` (оригинал не теряется); commit-ошибка оборачивается с контекстом.
- Read-modify-write в SQL — `SELECT ... FOR UPDATE` (флаг `forUpdate bool` в общем query-пути) — иначе параллельные транзакции молча перезаписывают друг друга. Документные БД — нативная `RunTransaction`. In-memory — полный mutex + commit копией значения.
- Общий код tx/non-tx путей: узкий приватный интерфейс `sqlContextGetter` (его реализуют и `*sqlx.DB`, и `*sqlx.Tx`) либо переданное замыкание; публичный метод — тонкая обёртка над одной приватной реализацией.
- Upsert идемпотентен (`INSERT ... ON DUPLICATE KEY UPDATE` / `ON CONFLICT`). Время в БД — **UTC**, локализация только при unmarshal.

### Маппинг и ошибки

- На каждый адаптер — свой приватный transport struct с тегами (`mysqlHour{Hour time.Time \`db:"hour"\`}`); в домен — только через валидированные конструкторы / `UnmarshalXFromDatabase` фабрики. Доменные значения литералом из данных БД не собираются.
- Driver not-found (`errors.Is(err, sql.ErrNoRows)`, `codes.NotFound`) для GetOrCreate-чтений → фабричный дефолтный агрегат, **не ошибка** («дата существует, даже если не сохранена»); для обычного Get → доменная `NotFoundError{uuid}`. Остальные инфраструктурные ошибки оборачиваются с контекстом (`"unable to get hour from db"`). Driver-ошибки выше repo не протекают.

### In-memory реализация

Обязательна для каждого интерфейса: `map[K]V` хранит **значения, не указатели** (никто не мутирует в обход `UpdateX`), чтение возвращает адрес копии, `*sync.RWMutex`; конструктор паникует на zero-зависимости. Domain-first: новый домен пишется против in-memory с unit-тестами, выбор хранилища откладывается (timebox).

### Secure by design

- Каждый метод чтения/мутации user-owned агрегата принимает **acting user явным типизированным параметром** — компилятор enforces то, что раньше держалось на ревью (кейс Harbor: пропущенный `if` = privilege escalation).
- Правило — в домене, enforcement — в repo: guard выполняется на свежепрочитанном агрегате внутри транзакции; ни один код-путь не вернёт/не сохранит агрегат без проверки.
- Ошибки отказа — экспортируемые структуры с контекстом: `ForbiddenToSeeTrainingError{RequestingUserUUID, TrainingOwnerUUID}`.
- Коллекции: ownership — в предикате запроса (`WHERE user_uuid = ...`), не пост-фильтрацией в app; небольшое дублирование правила — осознанный KISS-трейдофф, абстракция rule→query — только когда правило реально усложнится.
- **Без fake users** для системных потоков: явные роли с задекларированными правами; для userless-потоков (event-хендлеры, миграции) — отдельные методы repo с именами, кричащими о security-импликации; при расхождении поведения по акторам — отдельная CQRS-команда (`UpdateTrainingByOperations`).
- Ничего обязательного — через `context.Context`: identity = явный параметр. Желание протащить значение через ctx = сигнал декомпозировать функцию.

---

## 5. CQRS

### Базовая модель

- Command мутирует и **не возвращает бизнес-данных** (error — нормально); Query возвращает данные и **ничего не мутирует** (логи/метрики не считаются). Нарушение — только при полном понимании трейдоффа, как last resort.
- Один use case = один файл = struct команды (несёт **все** данные, доменными типами — `training.User`, не сырые строки: типобезопасность против перепутанных аргументов) + `<Name>Handler` с единственным методом `Handle(ctx context.Context, cmd <Name>) error`.
- Имена — бизнес-язык: `ScheduleTraining`, `CancelTraining`. Имя на `Create/Update/Delete` требует явного обоснования.
- `app.Application{Commands, Queries}` — единый каталог всех use cases модуля, собирается в composition root, инжектится во все ports. Ports зовут только `h.app.Commands.X.Handle(...)` — никогда адаптеры/repo напрямую.
- Командный слой **не пропускается** «для простых случаев»: тип стоит 3 минуты, его отсутствие — maintenance навсегда. Внутри монолита command bus не нужен — целевой app-слой зовётся напрямую.

### Хендлеры

- Command handler — чистая оркестрация: load через repo (updateFn), вызов behavior-методов домена, вызов внешних сервисов в правильном порядке, persist. Решение «можно ли» — всегда в домене.
- Зависимости хендлера — consumer-side интерфейсы в пакете command/query; конструктор паникует на nil.

### Декораторы (cross-cutting)

Generic-декораторы в `internal/common` оборачивают каждый command/query handler единообразно: logging (книжный идиом — `defer` на named `err`: `defer func() { logs.LogCommandExecution(name, cmd, err) }()`), metrics (duration, success/failure), tracing (span на каждый Handle). Современная замена logrus-идиомам — slog + OTel в тех же декораторах: cross-cutting живёт в одном месте, не в портах.

### Read models

- Query handler объявляет интерфейс read model рядом с собой (`AvailableHoursReadModel`); откуда данные — приложению «полностью прозрачно и неважно». Старт — та же БД, что у write-модели; отдельный read store (проекции, Elastic) добавляется позже за тем же интерфейсом. **Решение откладывается до конкретной нужды.**
- Query-типы — UI-shaped, отдельные от домена, OpenAPI-структур и DB-моделей. Не всякому query нужен read model — взять `hour.Repository` напрямую для простого предиката — нормально.
- POST-создание: UUID генерирует клиент/порт и кладёт в команду; ответ — `204 No Content` + `content-location` header. Работает без изменений, если команда позже станет асинхронной.

### Границы применимости

CQRS/Clean Architecture **не применять** к авторизации (read-write по природе) и тривиальным data-in-data-out CRUD-модулям — документировать исключение, пересматривать при росте логики.

---

## 6. События и интеграция контекстов

### Два вида событий

- **Domain events** — богатые, внутренние, живут в `domain/`.
- **Integration events** — плоские публичные контракты в `internal/<ctx>/events/`: exported-поля, JSON-теги, только примитивы + `time.Time`, **версия в имени**: `OrderPlacedV1`. Маппинг domain→integration — в app/adapters при публикации. Опубликованные схемы **immutable**: изменение = `OrderPlacedV2` + dual-publish на время миграции; V1 не редактируется никогда.

### Outbox и шина

- Публикация — через **transactional outbox**: событие пишется в той же БД-транзакции, что и изменение агрегата; `watermill-sql` (Postgres, `DefaultPostgreSQLSchema` + `DefaultPostgreSQLOffsetsAdapter`, `InitializeSchema: true`) превращает таблицы в durable at-least-once pub/sub — production-шина монолита без Kafka. Вынос на Kafka позже = замена publisher/subscriber, контракты и хендлеры не трогаются (Watermill forwarder — мост).
- **Один `message.Router` на бинарь**, middleware в фиксированном порядке: `CorrelationID` → `PoisonQueue(deadLetterPublisher, "events.dead_letter")` → `Retry{MaxRetries: 5, exp backoff до 30s}` → `Recoverer`. Recoverer — внутри (паника становится ошибкой для Retry); PoisonQueue — снаружи Retry (исчерпавшее ретраи сообщение уходит в dead-letter, не блокируя упорядоченный поток — SQL/Kafka-подписчики ordered, одно вечно-падающее сообщение остановит партицию). `plugin.SignalsHandler`; `router.Run(ctx)` в errgroup рядом с HTTP-сервером; readiness гейтится на `router.Running()`.

### Типизированные хендлеры и идемпотентность

- Watermill CQRS-компонент: `cqrs.NewEventBusWithConfig` / `cqrs.NewEventProcessorWithConfig`, generic `cqrs.NewEventHandler("OnOrderPlaced", func(ctx, e *ordersevents.OrderPlacedV1) error)`, `cqrs.JSONMarshaler{GenerateName: cqrs.StructName}`. Контекст регистрирует свои хендлеры через фасад (`svc.RegisterEventHandlers(processor)`).
- At-least-once ⇒ **каждый event handler идемпотентен**: upsert по натуральному ключу/event ID, дедупликация повторной доставки — обязательное свойство, проверяемое тестом.
- Проекции чужих данных наполняются только подпиской на чужие `events/`; sync-вызов — только через consumer-defined интерфейс + фасад (раздел 2). Sync/async выбирается **по природе бизнес-процесса**, не по удобству; «везде синхронно» запрещено.

---

## 7. Тесты

### Таксономия (таблица — в доках репо, убивает терминологические споры)

| Уровень | Docker DB | Внешние системы | Бизнес-фокус | Моки | Тестируемый API |
|---|---|---|---|---|---|
| Unit | нет | нет | зависит от кода | большинство зависимостей | Go package |
| Integration | да | нет | нет | обычно нет | Go package |
| Component | да | нет | да | только внешние сервисы | HTTP/gRPC |
| E2E | да | да | да | нет | HTTP |

Форма распределения — по профилю модуля: пирамида для logic-heavy, «ёлка» (integration-heavy) для IO/агрегационных. Слои тестов осознанно **перекрываются**. Цель — не 100% покрытия (70–80% — хорошо для Go), критерий — «насколько легко это сломать?». Бюджет: полный локальный прогон < 1 мин (цель <10 c).

### Domain (unit)

Максимальное покрытие corner cases, **ноль моков и инфраструктуры**; black-box принудительно — пакет `hour_test` (`_test` suffix); table-driven, snake_case-имена кейсов; sentinel-ошибки ассертятся равенством; фикстуры строятся **только** через доменный API (`newCanceledTraining(t)`, `MustNewUser` для краткости); `go-cmp` + `cmp.AllowUnexported` для приватных полей. Тесты, зеркалящие реализацию чистой функции, — не писать.

### App (unit)

Тестируется только реальная оркестрация (порядок вызовов, прокидывание значений); чистый glue — не тестируется. Моки — рукописные recording spies (struct с slice-полем), `newDependencies()`-хелпер; **никогда не тестировать мок** («метод был вызван» — не ассерт). Бизнес-сценарии в app-тестах = логика утекла из домена.

### Integration (адаптеры против реальной инфраструктуры)

- Реальные БД из docker-compose, образ **запинен на прод-версию**. Проверяем «правильно ли МЫ используем БД», включая транзакции.
- `t.Parallel()` в каждом тесте и сабтесте; **один shared black-box suite на все реализации** repo (real DB + in-memory) через `createRepositories(t)`.
- Изоляция уникальными данными (UUID / sync.Map-генератор), **cleanup запрещён**; ассертить наличие конкретного ID, никогда — длину коллекции. `sleep`, retries — запрещены; синхронизация каналами/WaitGroup, крайний случай — `assert.Eventually`.
- Обязательные тесты на каждый транзакционный repo: **rollback-тест** (updateFn мутирует и возвращает ошибку → перечитать → старое состояние живо) и **race-тест** (20 горутин, освобождаемых `close(startWorkers)`, победители в буферизованный канал, `assert.Len(..., 1)`). Всегда `-race`.
- **Test sabotage**: для non-TDD тестов сложного поведения — сломать реализацию (finishTransaction всегда коммитит) и убедиться, что suite падает.

### Component (один сервис целиком)

Два composition root: `NewApplication` (прод) и `NewComponentTestApplication` (внешние сервисы — моки), оба делегируют одному приватному `newApplication`. `TestMain` + HTTP-сервер в горутине + `WaitForPort` (никогда sleep). Реальные ports + реальная Docker-БД; **только happy path** — corner cases уже покрыты ниже. Клиенты — codegen из OpenAPI/proto, обёрнутые в `tests/client.go`-хелперы (принимают `*testing.T`, ассертят внутри, возвращают значимое: `trainingUUID := client.CreateTraining(t, ...)`). Auth — `FakeAttendeeJWT`-хелперы через реальный auth-путь, без bypass-флагов.

### E2E

Несколько коротких critical-path флоу: те же бинари, что в прод, docker-compose, только публичные HTTP-endpoints (внутренний RPC — лишь для сидинга данных). Проверяют wiring/контракты, **не логику**. Частые падения e2e = кто-то сломал контракт.

### Дисциплина

`require.*` для error/nil-гейтов, `assert.*` для значений, сообщения у неочевидных ассертов; повторяющиеся мультишаговые проверки → `assertXInRepository`-хелперы с `t.Helper()`, верификация round-trip-ом через публичный интерфейс. Loop-var capture: `c := testCases[i]` (или Go 1.22+). `make test` — единая точка входа локально и в CI (с passthrough `-run`); тесты вне CI = гниение гарантировано. Тесты необходимы, но недостаточны: пара — rollback/revert/undo-миграций + observability.

---

## 8. Observability

> Книга использует logrus и самодельный `logs.LogCommandExecution`; по правилу «modern tooling wins» — заменяем на slog + OpenTelemetry, сохраняя книжную архитектурную позицию: cross-cutting живёт в декораторах и middleware, не размазан по хендлерам.

### Логи — `log/slog`

- JSON-handler в проде, text — локально; один логгер собирается в `internal/common`, инжектится явно (не глобально).
- Обязательные атрибуты каждой записи: `trace_id`/`span_id` (из OTel-контекста через slog.Handler-обёртку), `correlation_id` (Watermill `CorrelationID` middleware), модуль/контекст, имя команды/запроса.
- Command/query-декоратор логирует каждый `Handle` единообразно (имя, длительность, err) — независимо от порта (HTTP/event/CLI). Это аудит-трейл «кто что исполнил».
- Никаких `fmt.Println`/log.Printf в слоях; домен не логирует вообще (чистота).

### Трейсы — OpenTelemetry

- `otelhttp` на HTTP-входе, instrumentation Watermill-хендлеров (span на обработку сообщения, propagation через message metadata), `otelsql`/instrumented driver на БД.
- Tracing-декоратор открывает span на каждый command/query handler; имя span = имя use case (`commands/ScheduleTraining`) — трейс читается как каталог `app.Application`.
- Context propagation сквозной: HTTP → command → outbox-publish → event handler (correlation/trace metadata в сообщении) — путь события через контексты виден одним трейсом.

### Метрики

- OTel metrics / Prometheus. Базовый набор **RED per use case**: rate, errors, duration по каждому command/query handler (метки: context, handler, result) — даёт metrics-декоратор автоматически для всех use cases.
- Инфраструктурные: HTTP-метрики (otelhttp), пул БД (`sql.DBStats`), Watermill — lag обработки, retries, **размер dead-letter топика** (алерт > 0), Go runtime metrics.
- Health: `/healthz` (liveness) и `/readyz` — readiness гейтится на `router.Running()` + ping БД.

### Дашборды и алерты

- На контекст-модуль: RED-панель команд/запросов, error-rate по slug-ошибкам, p95/p99 latency.
- Шина: dead-letter (алерт немедленно), retry-rate, возраст самого старого необработанного сообщения (lag outbox).
- Принцип Джесси Роббинса из книги: «сервис не протестирован, пока не сломан в проде» — observability + быстрый rollback/revert/undo-миграций равноправны тест-сьюту.

---

## 9. Антипаттерны — чего НЕ делать (из книги)

1. **«Java в Golang»** — перенос OOP-паттернов 1:1; domain service-объекты вместо простых функций.
2. **Anemic model**: data bags с сеттерами/геттерами; проверка состояния на стороне вызывающего + `SetState(...)`; pointer-флаги (`*string`, `*time.Time`) как состояние; magic strings ролей.
3. **Eight-thousanders**: god-хендлеры со «стеной if-ов»; CRUD-эндпоинт `UpdateX(bool, bool)` вместо behavior-команд.
4. **DB-теги/ORM-аннотации на доменных типах**; database-centric/response-centric дизайн; один struct на DB+API+домен (инцидент `LastIp`); «починка» мутацией модели перед сериализацией (`user.LastIp = nil`).
5. **DRY на данных**: общий тип для потребителей, меняющихся по разным причинам.
6. **Транзакции через context/middleware**; мутация переданного в updateFn указателя вместо persist возвращённого; забытый `FOR UPDATE`; потеря исходной ошибки при упавшем rollback.
7. **Per-use-case методы на repo**; валидация/логика в репозитории; «no rows» как ошибка там, где сущность логически существует; указатели в map in-memory repo.
8. **Guard-if авторизация по хендлерам** (Harbor-баг); fake users для системных операций; identity/обязательные входы через `context.Context`.
9. **Бизнес-if в app-слое**; HTTP-статусы из use cases вместо slug-ошибок; app импортирует adapters; всё в одном пакете «чтоб без вложенности» (Mattermost `app`).
10. **Микросервисы как лекарство от сложности**: нарезка по сущностям/эндпоинтам без доменного анализа → distributed monolith (та же связность + сеть + тулинг); «инфраструктура компенсирует дизайн» (Kubernetes при гнилом дизайне); синхронная коммуникация для асинхронных по природе процессов.
11. **Тактический DDD без стратегического** («super horrifying»); воркшопы без стейкхолдеров; PM в одиночку нарезает скоуп; границы по «похожести», а не по ответственности.
12. **BDUF, код «на будущее», перфекционизм**; месячные refactoring-ветки без ежедневной интеграции.
13. **Тестовые грехи**: ice-cream cone (e2e как основа); тесты вне CI; retries и растущие sleep как лечение флаков; cleanup данных; assert длины коллекции в параллельных тестах; тестирование моков и pass-through адаптеров; тесты-зеркала реализации; loop-var capture в `t.Parallel()` (молча зелёные тесты); 100%-coverage-карго-культ; терминологические дебаты вместо таблицы-таксономии.
14. **Вера, что строгий контракт (gRPC) гарантирует качество данных** — непустая бессмыслица всё равно проходит; валидация нужна на уровнях contract / contract-tests / e2e.
15. **Самописная аутентификация** (OWASP #2) и «временные» бэкдоры.
16. **Срезание углов как привычка** (а не осознанное прагматичное решение) и его зеркало — **полный стек паттернов на тривиальный CRUD**.

---

## 10. Чек-лист эталона (контракт ревью)

### Слои и модули

1. Репозиторий устроен как: `cmd/monolith/main.go` + `internal/common/` + `internal/<context>/{domain,app,ports,adapters,events,service}`; один `go.mod`; `pkg/` отсутствует.
2. Направление зависимостей: domain → ничего; app → только domain; ports/adapters → внутрь; ports не импортирует adapters. Проверяется линтером (`go-cleanarch`-класс) в CI.
3. Запрещён импорт чужого `domain/` и доступ к чужим таблицам; единственный импортируемый снаружи пакет контекста — `events/` (+ фасад из `service/` через consumer-side интерфейс).
4. `internal/common` не содержит ни одного бизнес-типа; общие бизнес-понятия дублируются по контекстам.
5. Все зависимости app-слоя — маленькие consumer-side интерфейсы, объявленные рядом с потребителем; имена — бизнес-концепты.
6. Композиция — только в main/`service/`; конструкторы `NewXxx` принимают интерфейсы и паникуют на nil-зависимость.
7. У каждого слоя — своя модель (storage struct / domain type / transport DTO); маппинг явный, в адаптере; общий struct между слоями — review-blocking defect. DRY применяется к поведению, не к данным.

### Домен

8. Все поля доменных типов unexported; создание — только через валидирующий конструктор `NewX(...) (*X, error)`; валидация — ровно в одном месте.
9. Публичный API домена — behavior-методы на языке бизнеса + предикаты; сеттеров и `GetX`-геттеров нет; guard инварианта и переход состояния — атомарно в одном методе.
10. Нарушения инвариантов — экспортируемые sentinel-ошибки (`var ErrX = errors.New(...)`); вызывающие не перепроверяют то, что домен уже гарантирует.
11. Доменные пакеты не импортируют БД/ORM/transport и не несут db/json-тегов; `context.Context` в сигнатурах repo — единственное допущение.
12. Перечислимые состояния — value-object типы с конструктором из сырого значения и `IsZero()`; `default` в switch по закрытому enum паникует.
13. Stateless-вычисления — простые функции доменного пакета; расчёт отделён от мутации.
14. Бизнес-`if` в app-слое или порту — дефект: переносится в домен.

### Repository

15. Интерфейс repo — в доменном пакете, минимальный (`Add/Get/Update`); без per-use-case методов; `ctx` — первый параметр.
16. Мутации — только через `UpdateX(ctx, id, user, updateFn func(ctx, *X) (*X, error)) error`: адаптер ведёт транзакцию, persist-ит возвращённое значение, откатывается на ошибке замыкания. Транзакции через context/middleware запрещены.
17. Репозиторий «глупый»: load → map → guard → persist, ноль валидации и бизнес-правил.
18. SQL: named err + `defer finishTransaction(err, tx)`; `multierr.Combine` при упавшем rollback; `SELECT ... FOR UPDATE` для read-modify-write; время в БД — UTC; upsert идемпотентен.
19. Driver not-found маппится в доменный дефолт (GetOrCreate) или доменную `NotFoundError`; все инфраструктурные ошибки оборачиваются с контекстом; driver-ошибки выше repo не протекают.
20. У каждого интерфейса repo есть in-memory реализация (map значений, RWMutex); новый домен разрабатывается domain-first против неё.

### Security

21. Методы repo для user-owned агрегатов принимают acting user явным типизированным параметром; identity из `context.Context` внутри repo не извлекается.
22. Авторизационное правило — чистая доменная функция `CanUserSee...(user, x) error`; repo вызывает её внутри транзакции до updateFn/возврата.
23. Системные потоки не используют fake users: явные роли либо отдельные методы/команды с говорящими именами (`UpdateTrainingByOperations`).
24. Ничего обязательного через `context.Context`; context values — только unexported key types + типизированный accessor `(T, error)`.
25. Аутентификация не самописная: provider/стандартный JWT, один middleware, типизированный `User` в контексте.

### CQRS

26. Каждый write use case — команда (struct с доменными типами) + `<Name>Handler.Handle(ctx, cmd) error`, не возвращающий бизнес-данных; каждый read — query handler без мутаций с UI-shaped результатами (не домен, не OpenAPI, не DB-модель).
27. Имена команд/запросов — бизнес-язык; `Create/Update/Delete` — только с обоснованием.
28. Единый `app.Application{Commands, Queries}` инжектится во все ports; ports зовут только хендлеры — никогда adapters/repo напрямую; командный слой не пропускается «для простоты».
29. Cross-cutting (логирование, метрики, трейсинг) — generic-декораторы из `internal/common` на каждом хендлере, единообразно для всех портов.
30. Ошибки app-слоя — ports-agnostic slug errors; ports транслируют их одним общим хелпером (HTTP) / canonical codes (gRPC); статусы из app/domain не возвращаются.
31. Create-эндпоинты: UUID генерируется на стороне клиента/порта, ответ `204` + `content-location`.
32. Read/write стартуют на одной БД; отдельный read store / async bus / event sourcing — отложенные решения за существующими интерфейсами.
33. CQRS/слои не применяются к тривиальному CRUD и auth; исключение задокументировано и пересматривается.

### События

34. Интеграционные события — плоские версионированные структуры в `events/` (`OrderPlacedV1`); схемы append-only: изменение = V2 + dual-publish.
35. Публикация — через transactional outbox (watermill-sql) в одной транзакции с изменением агрегата.
36. Один Watermill router на бинарь; middleware строго: CorrelationID → PoisonQueue(dead-letter) → Retry(exp backoff) → Recoverer; readiness гейтится на `router.Running()`.
37. Каждый event handler идемпотентен (at-least-once), это покрыто тестом; хендлеры типизированы через cqrs-компонент.
38. Sync/async классифицируется по природе бизнес-процесса; sync-вызов чужого контекста — только consumer-defined интерфейс + адаптер к фасаду.

### Тесты

39. В доках репо — таблица-таксономия 4 уровней (Docker DB / внешние системы / бизнес-фокус / моки / API); каждый тест классифицируем в ровно одну строку; имена уровней едины в пакетах, Makefile и CI.
40. Domain: black-box (`_test`-пакет), table-driven, ноль моков, corner cases; фикстуры — только через доменный API; `go-cmp`+`AllowUnexported`.
41. App: тестируется только оркестрация; моки — рукописные recording spies; тесты моков и pass-through адаптеров запрещены.
42. Один shared suite на все реализации repo; `t.Parallel()` везде; уникальные данные вместо cleanup; ассерты по конкретному ID; sleep/retries запрещены (`assert.Eventually` — крайний случай).
43. Обязательны rollback-тест и race-тест (N горутин, `close(start)`, ровно один победитель); весь прогон — `make test` с `-race`, одинаково локально и в CI; sabotage-проверка для тестов сложного поведения.
44. Component: два composition root (`NewApplication`/`NewComponentTestApplication` → общий `newApplication`), `TestMain`+`WaitForPort`, codegen-клиенты в `tests/client.go`, happy path only, auth через реальный путь (Fake-JWT-хелперы).
45. E2E: несколько коротких флоу, прод-бинари в docker-compose, только публичные endpoints, проверяют wiring/контракты.
46. Все уровни — в CI как merge-гейт; длительность пайплайна — бюджет (полный локальный прогон < 1 мин).

### Observability и операционка

47. Логи — slog (JSON в проде) с `trace_id`/`correlation_id`/именем use case; домен не логирует; каждый Handle логируется декоратором.
48. OTel-трейсинг сквозной: HTTP → handler-span → БД → outbox → event handler (propagation через message metadata).
49. Метрики RED per use case из metrics-декоратора + БД-пул, lag шины, размер dead-letter (алерт > 0); `/healthz` и `/readyz`.
50. Задокументированы быстрый rollback, revert и undo миграций — тест-сьют без них не считается достаточным.

### Процесс

51. Границы модулей — из доменного анализа (Event Storming-класс discovery), не из технической похожести; артефакт discovery хранится в репо, маппинг «событие/команда/агрегат → тип/хендлер» аудируем.
52. Литмус границы: новая фича реализуется и тестируется в одном модуле; иначе граница чинится до написания кода.
53. Имена пакетов/типов/методов/событий — Ubiquitous Language стейкхолдеров, без технических синонимов.
54. Инкременты — MVP-масштаба (~месяц); код «на будущее» отклоняется — вместо него дизайн, в котором будущее легко добавить; архитектурные рефакторинги интегрируются ежедневно, без долгоживущих веток.
55. Конфиг — только env vars с валидацией на старте; вся система поднимается одним `docker-compose up`; прод и локалка различаются конфигурацией адаптеров, не код-путями; контракты (OpenAPI/proto) — codegen через Makefile, `.gen.go` руками не правится.
