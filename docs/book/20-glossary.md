# Глоссарий

Термины сгруппированы по областям и отсортированы алфавитно по английскому названию внутри группы.
Каждая запись: 2–4 строки определения в контексте Molot, конкретика без «как правило, обычно», ссылка на файл и главу.

---

## I. Стратегическое проектирование

### Bounded Context (ограниченный контекст)

Семантическая граница, внутри которой конкретная модель и её язык непротиворечивы. В Molot
пять контекстов: `auction`, `billing`, `settlement`, `participant`, `notification`. Один и тот же
человек — `Bidder` в аукционе, `debtor` в биллинге, адресат рассылки в уведомлениях: три модели,
одна бизнес-сущность. Граница — не папка и не сервис; она охраняется компилятором и CI: импорт
чужого `domain/` или запрос к чужой Postgres-схеме валит сборку. Сдвинуть инвариант через
устоявшуюся границу — миграция данных плюс переиздание событий плюс переобучение команды.
→ Где смотреть: `internal/auction/`, `internal/billing/` · глава 2, глава 3.

### Context Map (карта контекстов)

Явная фиксация того, какие контексты существуют и как именно они связаны. Классические виды:
Shared Kernel, Customer–Supplier, Conformist, Anticorruption Layer, Open Host / Published Language.
В Molot settlement — Customer относительно auction и billing; между ними минимальный Anticorruption
Layer в `internal/settlement/adapters/`: типы settlement переводятся в `uuid.UUID`/`string` фасада,
ни один доменный тип auction не пересекает границу ни в одну сторону. Цена выноса контекста в
сервис при таком устройстве — замена одного файла адаптера на gRPC-клиент.
→ Где смотреть: `internal/settlement/adapters/`, `docs/adr/0004-settlement-saga.md` · глава 2.

### Event Storming (событийный штурм)

Метод discovery домена: поток доменных событий в прошедшем времени (BidPlaced, InvoiceExpired,
AuctionClosed) раскладывается хронологически; к событиям подбираются команды, акторы и политики.
Границы контекстов читаются там, где ломается язык и сменяется ответственность — не по
существительным, а по смене словаря. Правило 51 BOOK_AUDIT требует хранить артефакт штурма
в репозитории (`docs/event-storming.md`) и актуализировать его до кода при изменении домена:
сначала правится карта, потом код.
→ Где смотреть: `docs/event-storming.md` · глава 2.

### Ubiquitous Language (единый язык)

Словарь, на котором о домене говорят и стейкхолдеры, и код. В Molot нет ни одного имени
CRUD-типа — только доменные интенты: `ListAuction`, `PlaceBid`, `AwardToRunnerUp`,
`MarkSaleFailed`, `IssueInvoice`. Имя-интент фиксирует бизнес-операцию целиком вместе с
её guard-ами; имя-CRUD оставляет правила на совести вызывающего. Правило 53 BOOK_AUDIT делает
язык ревьюируемым артефактом: имена пакетов, типов, команд и событий — только из словаря домена,
и это проверяется на ревью, а не на слух.
→ Где смотреть: `internal/auction/app/command/` · глава 2.

---

## II. Тактическое проектирование

### Aggregate (агрегат)

Кластер объектов, который меняется как единое целое: одна транзакция — один агрегат (BOOK_AUDIT
§3). В Molot `Auction` — нетривиальный пример: история ставок входит в **границу согласованности**
(пишется в той же транзакции), но не загружается в память — хранятся только топ-2 ставки
денормализованно (`leadingBid`, `runnerUpBid`). Граница согласованности и объём данных в памяти —
разные вещи (ADR-0003). `Settlement` — второй агрегат-оркестратор: несёт состояние всего
платёжного процесса, переходы — явные guard-ящие методы.
→ Где смотреть: `internal/auction/domain/auction/auction.go`,
`internal/settlement/domain/settlement/settlement.go` · глава 5, глава 10.

### Always-valid model (always-valid модель)

Объект домена не может существовать в невалидном состоянии — ни сразу после создания, ни после
любого перехода. Три механизма: приватные поля, валидирующий конструктор `New(...) (*X, error)`,
behavior-методы с встроенными guard-ами (проверить и изменить — одна неделимая операция).
Следствие: вызывающему нечего забыть — нет публичных сеттеров, которые можно пропустить. Класс
багов «забыли проверить» не ловится на ревью — он не существует. Ср. anti-пример: `Auction{Status: "lsited"}` компилируется и сохраняется; с always-valid — нет.
→ Где смотреть: `internal/auction/domain/auction/auction.go`, `internal/auction/domain/auction/bid.go`
· глава 5.

### Composition Root (точка сборки)

Единственное место, где конкретные типы инстанциируются и соединяются друг с другом. В Molot —
`cmd/monolith/main.go` плюс `internal/<ctx>/service/service.go` каждого контекста. Публичная
функция `NewApplication` паникует на `nil`-зависимости: неправильная конфигурация взрывается при
старте процесса с читаемым сообщением, а не при первом запросе в prodution. Нет DI-фреймворка
с рефлексией — граф зависимостей читается статически, ошибки обнаруживаются компилятором.
→ Где смотреть: `cmd/monolith/main.go`, `internal/auction/service/service.go` · глава 6.

### Consumer-side interface (интерфейс у потребителя)

Интерфейс объявляет тот, кто им пользуется, а не тот, кто его реализует — образец: `io.Writer`
в stdlib. В Molot `bidderProfiles` и `clock` объявлены приватно в `internal/auction/app/command/deps.go`:
потребитель заказывает ровно те методы, которые нужны ему, — ни одного лишнего. Адаптер реализует
интерфейс неявно, без импорта пакета потребителя. Следствие: циклов импортов не существует
структурно; рукописный fake для теста — пара строк без моковых фреймворков.
→ Где смотреть: `internal/auction/app/command/deps.go`, `internal/billing/app/command/pay_invoice.go`
· глава 6.

### Invariant (инвариант)

Условие, истинное в любой момент существования агрегата. В Molot инварианты охраняются
guard-каскадами внутри behavior-методов: `PlaceBid` последовательно проверяет — открыт ли аукцион,
не продавец ли ставит, не лидер ли перебивает себя, та ли валюта, достаточна ли сумма, нужна ли
верификация. Порядок guard-ов — часть публичного контракта: `"guard order: closed auction wins over
below-minimum"` — отдельный тестовый кейс. Нарушение порядка — семантическое изменение API.
→ Где смотреть: `internal/auction/domain/auction/auction.go`,
`internal/auction/domain/auction/place_bid_test.go` · глава 5.

### Repository (репозиторий)

Паттерн, превращающий бизнес-операцию «прочитай-измени-сохрани» в атомарную и конкурентно-
безопасную так, чтобы ни домен, ни app-слой не знали, как именно это достигнуто. В Molot
минимальный интерфейс (`Add / Get / Update / UpdateAsSystem`) живёт в `domain/` — интерфейс
объявляет владелец инварианта. Мутации проходят только через `updateFn`-замыкание; транзакция —
собственность адаптера; `*sql.Tx` выше repo не вытекает (правило 16 BOOK_AUDIT).
→ Где смотреть: `internal/auction/domain/auction/repository.go`,
`internal/auction/adapters/auction_pg_repository.go` · глава 7.

### Sentinel error / Slug error (sentinel-ошибка / slug-ошибка)

**Sentinel-ошибка** — именованная переменная пакета домена (`var ErrBidBelowMinimum = errors.New(...)`):
словарь, на языке которого вызывающий ветвится через `errors.Is`. `"validation failed"` — не sentinel:
теряет семантику. **Slug-ошибка** — обёртка app-слоя с kind и slug (`errs.NewConflictError("bid-below-minimum").WithCause(...)`) для маппинга в HTTP-статус единым хелпером. Use case не знает про HTTP;
порт не знает про бизнес-правила. Два уровня: домен говорит «что не так», порт переводит в статус.
→ Где смотреть: `internal/auction/domain/auction/errors.go`, `internal/common/errs/` · главы 5, 16.

### updateFn (замыкание мутации)

Каноническая сигнатура мутации в репозитории Molot: `func(ctx context.Context, a *X) (*X, error)`.
Адаптер гарантирует строгую последовательность внутри одной транзакции: load (`SELECT ... FOR UPDATE`) →
guard доступа → вызов fn → persist возвращённого значения (инкрементируя version). `return a, nil`
читается как «вот это сохрани»; `return nil, err` — «откати всё, ничего не сохраняй». Замыкание
возвращает агрегат, а не мутирует входной указатель: явный коммит намерения без гадания
«надо ли сохранять после частичной ошибки».
→ Где смотреть: `internal/auction/app/command/place_bid.go`,
`internal/auction/adapters/auction_pg_repository.go` · глава 7.

### Value Object (value object, объект-значение)

Immutable-значение без идентичности, сравниваемое по содержимому. В Go — struct с приватными полями,
конструктором и методами-операциями. Идиома Molot: zero value = «не задано», проверяется через
`IsZero()`; никаких `*Money` с nil-семантикой — три состояния (nil / zero / значение) там, где
смысла два. VO переносит проверку из «места использования» в «место создания»: у вас в руках
`Money` — валюта уже валидна, сумма неотрицательна. `Add` guard-ит валюты явно; сложить евро с
долларами невозможно конструктивно. Тип — это доказательство.
→ Где смотреть: `internal/auction/domain/auction/money.go`,
`internal/billing/domain/invoice/money.go` · глава 5.

### Version (версия агрегата)

Целочисленное поле (`int64`) в строке агрегата, инкрементируемое при каждом `UPDATE`. В Molot
применяется в паре с `SELECT ... FOR UPDATE` как вторая линия защиты от lost update:
`WHERE id = $1 AND version = $N` с `version = $N + 1` в той же инструкции. При живом FOR UPDATE
этот конфликт недостижим — и именно поэтому он нужен: тот день, когда кто-то изменит строку
в обход лока (ops-правка, кривая миграция), превращается не в тихую перезапись данных, а в
громкую ошибку «optimistic lock conflict». Belt and suspenders: FOR UPDATE — каждый день,
version — в тот единственный день, когда FOR UPDATE обошли.
→ Где смотреть: `internal/auction/adapters/auction_pg_repository.go` · глава 7.

---

## III. Данные и транзакции

### Optimistic locking / Pessimistic locking — FOR UPDATE

Pessimistic locking (`SELECT ... FOR UPDATE`) — эксклюзивный row lock: вторая транзакция ждёт и
видит уже изменённые данные. Достаточно, пока каждый пишущий код-путь честно проходит через лок.
Optimistic locking (`WHERE ... AND version = $N`) — вторая линия: запись в обход лока даёт `0 affected rows`
вместо тихой перезаписи. В Molot оба применяются одновременно на каждой мутации агрегата;
коллизия версии в комментарии репозитория прямо названа «belt-and-suspenders to FOR UPDATE».
→ Где смотреть: `internal/auction/adapters/auction_pg_repository.go`,
`internal/settlement/adapters/settlement_pg_repository.go` · глава 7.

### Outbox — transactional outbox (транзакционный ящик исходящих)

Паттерн устранения dual-write: интеграционное событие записывается в ту же Postgres-транзакцию,
что и мутация агрегата — в таблицу-«исходящие». Либо коммитятся оба, либо откатываются оба.
Альтернативы — CDC (читать WAL) и event sourcing — отклонены в пользу outbox: даёт бизнес-события,
не дифф строк; не требует новой инфраструктуры. В Molot реализован через watermill-sql: outbox-таблица
сама является топиком, отдельный relay не нужен. Публикация — `INSERT` в outbox через тот же `*sql.Tx`.
→ Где смотреть: `internal/auction/adapters/auction_pg_repository.go` (`publishMapped`),
`internal/common/watermill/pubsub.go` · глава 9.

### Projection / Read model (проекция / модель чтения)

Денормализованная view, сформированная под конкретный экран или use case чтения. В Molot query
handler возвращает `CatalogPage`, `AuctionCardView`, `DashboardView` — структуры под UI, не доменные
агрегаты. Проекция может строиться прямым запросом к write-таблицам (строгая консистентность) или
отдельной таблицей, обновляемой по событиям (`bidder_profiles` — локальная проекция participant-событий
внутри auction-схемы, eventual consistency). READ-запросы не проходят через Repository агрегата.
→ Где смотреть: `internal/auction/app/query/active_catalog.go`,
`internal/auction/adapters/auction_pg_read_models.go`,
`internal/auction/adapters/bidder_profiles_pg.go` · глава 8.

### UnmarshalFromDatabase (фабрика гидрации)

Фабрика агрегата для пути «из БД» — единственная альтернатива `New(...)`. В Molot
`auction.UnmarshalFromDatabase(...)` принимает значения, уже прошедшие через VO-конструкторы:
`NewStatusFromString`, `UnmarshalBiddingWindow`, `NewMoney`. Битая строка в БД (ручная правка,
кривая миграция) превращается в явную ошибку на загрузке, а не в паническое поведение тремя
вызовами позже. Repository — граница доверия: данные из БД не более доверенные, чем данные из
HTTP-запроса, и проходят ту же валидацию.
→ Где смотреть: `internal/auction/domain/auction/auction.go`,
`internal/auction/adapters/auction_pg_repository.go` · главы 5, 7.

---

## IV. События и интеграция

### At-least-once delivery (доставка «как минимум один раз»)

Гарантия: каждое сообщение будет доставлено хотя бы один раз, дубликаты возможны. «Exactly-once»
между двумя независимыми системами недостижим без общей транзакции — это at-least-once плюс
дедупликация на стороне потребителя. В Molot at-least-once реализован Watermill Retry (5 попыток,
экспоненциальный backoff с cap 30 с); идемпотентность — обязанность каждого подписчика, не опция:
правило 9 TEXTBOOK фиксирует это как золотое.
→ Где смотреть: `internal/common/watermill/watermill.go` · глава 9.

### Compensation (компенсация)

Действие, семантически отменяющее уже закоммиченный шаг саги, если процесс не может продолжиться
вперёд. Не технический rollback — новый бизнес-шаг. В Molot компенсации — доменные решения:
`AwardToRunnerUp` (передать лот второму бидеру), `RelistAuction` (перевыставить лот),
`MarkSaleFailed` (признать продажу несостоявшейся). Каждый фасад идемпотентен: «уже в целевом
состоянии» → соответствующий sentinel → no-op для саги, ретрай безопасен конструктивно.
→ Где смотреть: `internal/settlement/domain/settlement/settlement.go` (`nextStepOnFailure`),
`internal/auction/service/facade.go` · глава 10.

### Correlation ID (идентификатор корреляции)

Сквозной идентификатор, связывающий HTTP-запрос, команды, outbox-события и event handler-ы в одну
трассируемую цепочку. В Molot `correlation_id` устанавливается Watermill `CorrelationID` middleware
первым в цепочке — до PoisonQueue, чтобы dead-letter тоже содержал id — и добавляется в каждую
slog-запись через `NewContextHandler`. Идёт в metadata сообщения, не в payload: транспортные
заботы отделены от бизнес-контракта. Совместно с `trace_id` — jump от лога к трейсу в три клика.
→ Где смотреть: `internal/common/logs/logs.go`, `internal/common/watermill/watermill.go` · глава 14.

### Crash seam (шов падения)

Точка между двумя эффектами, где процесс может умереть, оставив первый эффект без второго.
В платёжном протоколе Molot три шва: между `Charge` и записью статуса, между записью и ответом
клиенту. В settle-саге: между `IssueInvoice` и `InvoiceIssued`-коммитом; между `AwardToRunnerUp`
и коммитом `AwardingRunnerUp` и т.д. Каждый шов перечислен в ARCHITECTURE §6.7 и закрыт
отдельным тестом с `sagaRepoSpy`: скриптованный сбой → redelivery → проверка корректного
терминала без Docker.
→ Где смотреть: `internal/settlement/app/handlers_test.go`, `docs/ARCHITECTURE.md` §6.7 · глава 10.

### Dead letter (очередь мёртвых сообщений)

Хранилище сообщений, которые не смогли обработаться после всех попыток ретрая. В Molot —
таблица `watermill_events_dead_letter`; сообщение попадает туда после 5 ретраев с экспоненциальным
backoff. Значение > 0 — всегда инцидент, никогда «бизнес-шум» (ARCHITECTURE §6.8). Метрика
`molot_bus_dead_letter_size` — stat-панель с красным фоном; алерт на size > 0 — немедленный.
Бизнес-невалидные сообщения (неизвестный тип, нарушение guard-а) должны ack-аться кодом
и в dead-letter не попадать вовсе.
→ Где смотреть: `internal/common/watermill/watermill.go` (PoisonQueue),
`docs/ARCHITECTURE.md` §6.8 · глава 9, глава 14.

### Domain Event vs Integration Event (доменное vs. интеграционное событие)

**Доменное событие** — богатое, внутреннее: несёт доменные типы (`Bid`, `ClosingResult`),
записывается behavior-методом агрегата, живёт в `domain/`. **Интеграционное событие** — плоское,
версионированное, публичный контракт: только примитивы и `time.Time`, суффикс `V1` в имени
(`AuctionClosedV1`), живёт в `events/` — единственном пакете контекста, разрешённом к импорту
снаружи. Менять поле `V1` запрещено навсегда: нужна новая структура `V2` плюс dual-publish,
пока жив хоть один V1-подписчик.
→ Где смотреть: `internal/auction/domain/auction/events.go`, `internal/auction/events/events.go`,
`internal/auction/adapters/events_mapper.go` · глава 9.

### Fast-forward (перемотка саги вперёд)

Ситуация, когда событие-«доказательство» (факт совершённого эффекта) приходит в сагу, которая
ещё не зафиксировала соответствующий переход из-за краша между effect и commit. В Molot
`WinnerReassignedV1`, пришедший в состоянии `AwaitingPayment`, доказывает: `AwardToRunnerUp`
уже выполнен в auction. Guard `DecideOnWinnerReassigned` принимает это как легитимный fast-forward
и переходит к `IssueInvoice(attempt=2)` без ошибки. Без fast-forward событие было бы ack-нуто —
сага зависла бы навсегда, и никто не заплатил бы продавцу.
→ Где смотреть: `internal/settlement/domain/settlement/settlement.go`,
`internal/settlement/app/handlers_test.go` · глава 10.

### Idempotency (идемпотентность)

Свойство операции: повторный вызов с теми же аргументами даёт тот же результат, не создавая
дополнительных эффектов. В Molot многоуровневая: `ON CONFLICT (id) DO NOTHING` на `Add` агрегата
(client-generated UUID), `ErrAlreadyClosed` → `nil` в воркере закрытия, `UNIQUE(auction_id, attempt)`
для `IssueInvoice`, `Refund` — no-op при отсутствии списания. Идемпотентность фасадов settlement —
не соглашение, а типизированный контракт: «уже в целевом состоянии» возвращает доменный sentinel,
который handler обязан смапить в `nil`.
→ Где смотреть: `internal/auction/app/command/close_auction.go`,
`internal/auction/app/command/award_to_runner_up.go` · главы 9, 10, 16.

### Process Manager / Saga (менеджер процессов / сага)

**Сага** — последовательность коротких локальных транзакций, каждая коммитится самостоятельно, плюс
компенсации. Гарантия слабее 2PC: не «всё или ничего», а «процесс всегда довершится». **Process
Manager** — агрегат-оркестратор, явно хранящий состояние процесса и раздающий команды. В Molot
`Settlement` несёт `state`, `winner`, `attempt`, `relistGen`, `invoiceID`; решения — value-receiver
чистые функции (`nextStepOnFailure`); переходы — pointer-receiver guard-ящие методы. Граница
«решить / изменить» проведена прямо в сигнатурах.
→ Где смотреть: `internal/settlement/domain/settlement/settlement.go`,
`internal/settlement/app/handlers.go`, `docs/adr/0004-settlement-saga.md` · глава 10.

### Side effect / Effect-фаза (эффект, эффект-фаза)

В протоколе саги effect-фаза — вызов внешнего фасада (`IssueInvoice`, `AwardToRunnerUp`) строго
**вне** транзакции settlement. Нельзя звать эффект внутри `updateFn`: чужая транзакция коммитится
под row-lock-ом саги — каскадные деградации и дедлоки. Порядок жёсткий: сначала эффект, потом
фиксация состояния (`repo.Update`). Crash между ними — штатный режим протокола: идемпотентность
каждого эффекта гарантирует, что redelivery корректно довершит транзакцию.
→ Где смотреть: `internal/settlement/app/handlers.go`, `docs/adr/0004-settlement-saga.md` · глава 10.

---

## V. Тестирование

### Command / Query — CQRS (разделение записи и чтения)

**Command** — намерение изменить состояние: `Handle` возвращает только `error`, никаких бизнес-данных.
**Query** — намерение прочитать: возвращает UI-shaped структуру, не агрегат. В Molot граница
проходит по типу через `app.Application{Commands, Queries}`. Команда может стать асинхронной без
изменения сигнатуры; read model эволюционирует независимо от write-side; query-адаптер можно
переписать или вынести в реплику без касания доменного кода. CQRS здесь — не шина и не
event sourcing, а разделение ответственностей в одном монолите.
→ Где смотреть: `internal/auction/app/app.go`, `internal/auction/app/command/place_bid.go`,
`internal/auction/app/query/active_catalog.go` · глава 8.

### Recording spy (записывающий шпион)

Рукописная структура, реализующая consumer-side интерфейс и записывающая факты каждого вызова
(аргументы, порядок, количество). В Molot `sagaRepoSpy` и `auctionGatewaySpy` в
`internal/settlement/app/spies_test.go` — in-memory реализации с дополнительными возможностями:
`updateErr` — скриптованный сбой коммита, `onUpdate`-хук — моделирование конкурентного коммита из
другой горутины. Отличие от mock-фреймворка: шпион проверяет содержимое вызовов, а не сам факт;
«`IssueInvoice` был вызван с `invoiceID` X, `attempt=1`» — ассерт, «`IssueInvoice` был вызван» — театр.
→ Где смотреть: `internal/settlement/app/spies_test.go`,
`internal/settlement/app/handlers_test.go` · глава 13.

### Shared suite (общий тест-сьют)

Один исполняемый тест-сьют на все реализации одного интерфейса. В Molot `runRepositorySuite(t, newRepo)`
вызывается из `inmem_repository_test.go` (unit, без Docker) и из `pg_integration_test.go` (реальный
Postgres, build tag `integration`). Сьют — контракт поведения: идемпотентность `Add`, rollback при
ошибке замыкания, инкремент версии, race-тест параллельных ставок. Расхождение семантики двух
реализаций — красный тест, обнаруженный в CI, а не сюрприз в production.
→ Где смотреть: `internal/auction/adapters/repository_suite_test.go`,
`internal/auction/adapters/inmem_repository_test.go`,
`internal/auction/adapters/pg_integration_test.go` · глава 13.

---

## VI. Эксплуатация

### RED metrics (RED-метрики)

Rate / Errors / Duration — три метрики на каждый use case: «сколько в секунду», «сколько ошибок»,
«как долго». В Molot генерируются автоматически через `ApplyCommandDecorators` /
`ApplyQueryDecorators` из `internal/common/decorator/decorator.go`: histogram
`molot_command_duration_seconds{context, handler, result}` появляется для каждого нового handler-а
без единой строки observability-кода в нём. Grafana-дашборд `molot-contexts-red` строит rate,
error rate и p95/p99 на каждый `{context, handler}`.
→ Где смотреть: `internal/common/decorator/decorator.go` · глава 14.

### SLO (Service Level Objective)

Численная цель по надёжности, определяющая, когда трейдофф «вынести контекст в отдельный сервис»
оправдан. В Molot SLO упоминается в ADR-0001 как критерий N 3: «изоляция отказа — требование SLA
на контекст принципиально отличается от остальных». Пока этот критерий не достигнут — границы
выгоднее держать в монолите, где их дёшево исправлять. Без явного SLO архитектурное решение о
деплое принимается по интуиции, а не по данным об отказах.
→ Где смотреть: `docs/adr/0001-modular-monolith.md`,
`docs/book/03-modular-monolith.md` · глава 3.

### Worker, time-based (воркер по времени)

Компонент порт-слоя внутри бинара, запускающий команды по таймеру без внешнего cron. Паттерн
Molot: `tick → lightweight scan (кандидаты по partial index) → команда на каждого`. Воркер не
проверяет состояние — guard агрегата решает всё. `ErrAlreadyClosed` и `ErrBiddingStillOpen`
маппятся в `nil`: это не «проглатывание ошибок», а явный контракт «для воркера эти исходы
доброкачественны». Scan использует `WHERE status = 'listed'` partial index — рост таблицы не
влияет на производительность скана.
→ Где смотреть: `internal/auction/ports/worker.go`, `internal/billing/ports/worker.go`,
`internal/auction/adapters/migrations/00001_auction_schema.sql` · глава 12.
