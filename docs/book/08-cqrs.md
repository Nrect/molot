# 8. CQRS: разделение моделей, а не инфраструктуры

## Зачем читать

Аббревиатура CQRS обросла таким количеством легенд, что большинство команд либо игнорирует идею целиком, либо приходит к ней в самой тяжёлой конфигурации — с отдельной шиной, отдельным хранилищем и event sourcing в придачу. Между этими полюсами есть практичная середина, которую Molot использует с первого дня без единого дополнительного брокера и без event sourcing. В этой главе разберём, что CQRS есть на самом деле, как его применять в разных сценариях и как распознать три ошибки, которые убивают идею на корню.

---

## Проблема

Представьте универсальный метод сервиса:

```go
// так в большинстве codebases
func (s *AuctionService) PlaceBid(ctx context.Context, req PlaceBidRequest) (*Auction, error) {
    a, err := s.repo.GetByID(ctx, req.AuctionID)
    if err != nil { return nil, err }
    if err := a.PlaceBid(req.BidderID, req.Amount); err != nil { return nil, err }
    if err := s.repo.Save(ctx, a); err != nil { return nil, err }
    return a, nil  // возвращаем агрегат — «зачем тратить лишний запрос»
}
```

Этот метод делает сразу три вещи с конфликтующими требованиями. Как хранилище данных он хочет нормализации и инвариантов. Как источник ответа для HTTP он хочет плоской структуры под конкретный экран. Как транзакционная единица он хочет минимального объёма данных в локе. Эти цели тянут код в разные стороны.

Дальше появляются методы с флагами: `GetAuction(ctx, id, withBids bool, withSeller bool)`. Потом кэшировать чтения оказывается невозможно — они сцеплены с мутирующей логикой. Оптимизация чтения рискует сломать запись, и наоборот. Тесты на логику ставки вынуждены работать с «полным» объектом, хотя им нужна только горстка полей.

---

## Что такое CQRS (и чем он не является)

Command Query Responsibility Segregation в минимальном определении — это **две модели вместо одной**: отдельная модель для записи (Command side) и отдельная для чтения (Query side). Больше ничего.

Это не:
- не event sourcing (CQRS работает с обычными реляционными таблицами);
- не обязательная шина команд (прямой вызов хендлера — полноценный CQRS);
- не отдельная база данных для чтений (read model можно строить на тех же таблицах);
- не eventual consistency по умолчанию (прямое чтение write-таблиц — строго консистентно).

Суть — в разделении ответственностей, а не в конкретной инфраструктуре. Это важно: многие команды откладывают CQRS до момента, когда «созреют» для шины и отдельного хранилища, хотя основную пользу — упрощение кода и развязку эволюции чтений и записей — можно получить немедленно.

---

## Как в Molot

### Единый каталог: `app.Application{Commands, Queries}`

Граница между write и read side проходит по типу:

```go
// internal/auction/app/app.go

// Commands — 8 write use cases (§3).
type Commands struct {
    ListAuction   decorator.CommandHandler[command.ListAuction]
    PlaceBid      decorator.CommandHandler[command.PlaceBid]
    CancelAuction decorator.CommandHandler[command.CancelAuction]
    CloseAuction  decorator.CommandHandler[command.CloseAuction]

    // Saga facade commands — called only through service.Facade.
    AwardToRunnerUp   decorator.CommandHandler[command.AwardToRunnerUp]
    RelistAuction     decorator.CommandHandler[command.RelistAuction]
    MarkSaleFailed    decorator.CommandHandler[command.MarkSaleFailed]
    ConfirmSettlement decorator.CommandHandler[command.ConfirmSettlement]
}

// Queries — 4 read use cases (§3).
type Queries struct {
    ActiveAuctionsCatalog decorator.QueryHandler[query.ActiveAuctionsCatalog, query.CatalogPage]
    AuctionCard           decorator.QueryHandler[query.AuctionCard, query.AuctionCardView]
    BidHistory            decorator.QueryHandler[query.BidHistory, query.BidHistoryPage]
    SellerDashboard       decorator.QueryHandler[query.SellerDashboard, query.DashboardView]
}
```

`Application` собирается один раз в `service/NewService` и инжектируется во все порты. Порты вызывают только хендлеры через этот каталог — никогда репозиторий или адаптер напрямую. Структура `Commands`/`Queries` сама по себе является артефактом архитектуры: открыв `app.go`, разработчик видит полный каталог use cases контекста.

### Команда: struct с доменными типами, Handle возвращает только error

```go
// internal/auction/app/command/place_bid.go

type PlaceBid struct {
    AuctionID auction.AuctionID
    BidID     auction.BidID
    Bidder    auction.BidderID
    Amount    auction.Money
}
```

Поля — доменные типы, не примитивы. `AuctionID` — не `string`, не `uuid.UUID`, а `auction.AuctionID`. Это не педантизм: опечататься и передать `BidID` туда, где ожидается `AuctionID`, невозможно — компилятор поймает.

Хендлер:

```go
// internal/auction/app/command/place_bid.go

func (h PlaceBidHandler) Handle(ctx context.Context, cmd PlaceBid) error {
    bidder, err := h.profiles.BidderByID(ctx, cmd.Bidder)
    if err != nil {
        return err
    }

    err = h.repo.Update(ctx, cmd.AuctionID, auction.ActorFromBidder(cmd.Bidder),
        func(ctx context.Context, a *auction.Auction) (*auction.Auction, error) {
            if _, err := a.PlaceBid(cmd.BidID, bidder, cmd.Amount, h.clock.Now()); err != nil {
                return nil, err
            }
            return a, nil
        })
    if err != nil {
        return mapPlaceBidError(err)
    }
    return nil
}
```

Обратите внимание: `Handle` возвращает только `error`. Никаких бизнес-данных. Это контрактное решение с важным следствием: если завтра `PlaceBid` нужно будет сделать асинхронным, сигнатура не меняется. HTTP-порт уже сейчас возвращает `204 + content-location` — клиент знает, где прочитать результат, не ожидая его от команды.

Аналогичное правило применяется к `ListAuction`. Продавец отправляет команду с клиент-сгенерированным UUID лота:

```go
// internal/auction/app/command/list_auction.go

func (h ListAuctionHandler) Handle(ctx context.Context, cmd ListAuction) error {
    a, err := auction.New(cmd.AuctionID, cmd.Seller, cmd.Lot,
        cmd.StartPrice, cmd.Increment, cmd.Reserve, cmd.Window,
        h.rules, h.clock.Now())
    if err != nil {
        return mapListingError(err)
    }
    if err := h.repo.Add(ctx, a); err != nil {
        return err
    }
    return nil
}
```

Клиент генерирует ID сам — это делает `Add` идемпотентным: повторный POST с тем же ID — тихий no-op. Сервер не занимается генерацией идентификаторов и не возвращает их обратно. Паттерн «client-generated UUID + 204» решает проблему дублирования при ретраях бесплатно.

### Запрос: UI-shaped результат, не доменный объект

Принципиальный момент: query handler не возвращает доменный агрегат. Он возвращает структуру, сформированную под конкретный экран.

```go
// internal/auction/app/query/active_catalog.go

type CatalogItem struct {
    AuctionID           string
    Title               string
    SellerID            string
    CurrentPriceMinor   int64
    MinimalNextBidMinor int64
    Currency            string
    BidCount            int
    EndsAt              time.Time
    // EndingSoon is computed by the adapter at read time
    // (ends_at - now() < 5 min) — never stored, it would go stale
    // between events (§5).
    EndingSoon bool
}
```

`CatalogItem` — не `Auction`, не OpenAPI-DTO из `openapi.gen.go`, не DB-строка. Это отдельная структура с полями ровно под каталожный список. Из неё убраны поля, которые каталогу не нужны (reserve, antisnipe policy, extensions detail), и добавлены поля, которые каталог хочет видеть готовыми (`MinimalNextBidMinor`).

Правило из аудита 26: результат query — UI-shaped, не домен, не OpenAPI, не DB-модель. Это требует трёх отдельных структур. Вместе с главой 3 (DRY относится к поведению, а не к данным) это объясняет, почему «лишние» структуры — не overhead, а плата за независимость слоёв.

### Generic-декораторы: один раз для всего

Вместо того чтобы в каждом хендлере писать `logger.Info(...)`, `metrics.Record(...)`, `span.Start(...)`, в Molot есть единая точка навески:

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

Декоратор строит цепочку: tracing (внешний слой, так что все logs и metrics несут span context) → logging → metrics (внутренний, измеряет чистое время хендлера). Имя use case извлекается через рефлексию из типа команды — `PlaceBid` становится `"commands/PlaceBid"` в трейсе. Каждый хендлер в каталоге `Commands`/`Queries` оборачивается один раз при сборке в `service/NewService`. Добавить новый хендлер — добавить одну строку с `ApplyCommandDecorators`. Добавить новый cross-cutting concern — добавить один декоратор.

Результат: бизнес-код хендлера (`PlaceBidHandler.Handle`) не содержит ни строчки логирования, метрик и трейсинга. Золотое правило 12: cross-cutting живёт в декораторах, не в хендлерах.

### Read models: два честных пути в одном проекте

Где именно хранятся данные для read side — решается на уровне адаптера, прозрачно для query handler. В Molot одновременно используются два подхода:

**Путь 1 — проекции (eventually consistent).**

`catalog_items` и `seller_dashboard_items` поддерживаются отдельными обработчиками событий. При каждом `BidPlacedV1` проекция каталога обновляется с монотонным guard-ом:

```go
// internal/auction/adapters/catalog_pg_projection.go

// ApplyBid advances price/count/deadline. The monotonic guard
// (bid_count < incoming) makes redeliveries and out-of-order updates
// no-ops; the increment for the minimal next bid is joined from the
// context's own write table (the event does not carry it).
func (p *CatalogPostgresProjection) ApplyBid(
    ctx context.Context,
    auctionID uuid.UUID, amountMinor int64, bidCount int, newEndsAt time.Time,
) error {
    res, err := p.db.ExecContext(ctx, `
        UPDATE auction.catalog_items ci SET
            current_price_minor = $2,
            minimal_next_bid_minor = $2 + a.increment_minor,
            bid_count = $3,
            ends_at = $4,
            updated_at = now()
        FROM auction.auctions a
        WHERE ci.auction_id = $1 AND a.id = $1 AND ci.bid_count < $3`,
        auctionID, amountMinor, bidCount, newEndsAt)
    ...
}
```

`WHERE ci.bid_count < $3` — это монотонный guard: повторная доставка события с тем же `bidCount` становится no-op. Без него при redelivery проекция могла бы откатить цену назад. Проекции подходят для данных с высокой читаемостью, где небольшая задержка допустима.

**Путь 2 — прямое чтение write-таблиц.**

`AuctionCard` и `BidHistory` читают напрямую из `auction.auctions` и `auction_bids`:

```go
// internal/auction/adapters/auction_pg_read_models.go

// AuctionCard reads the public lot page directly from the write
// tables. The reserve amount never leaves — only HasReserve.
func (m *AuctionPostgresReadModels) AuctionCard(ctx context.Context, id auction.AuctionID) (query.AuctionCardView, error) {
    var (
        view               query.AuctionCardView
        auctionID, seller  uuid.UUID
        outcome            sql.NullString
        reserveMinor       sql.NullInt64
        leadingBidder      uuid.NullUUID
        leadingAmountMinor sql.NullInt64
    )
    err := m.db.QueryRowContext(ctx, `
        SELECT id, title, description, seller_id, status, outcome,
               start_price_minor, increment_minor, reserve_price_minor, currency,
               starts_at, ends_at, extensions_used, bid_count,
               leading_bidder_id, leading_amount_minor
        FROM auction.auctions WHERE id = $1`, id.UUID()).Scan(...)
    ...
}
```

Это строго консистентно, не требует отдельной таблицы и проще в поддержке. `BidHistory` читает из `auction_bids` — append-only таблицы, которая и есть история в готовом виде.

**Как выбирать между двумя путями.** Правило аудита 32: read/write стартуют на одной БД; не всякому query нужен read model. Проекция оправдана, когда:
- запрос агрегирует данные из нескольких источников (каталог собирает поля из событий разных типов);
- нагрузка на чтение такова, что отдельный индекс или предагрегация заметно дешевле JOIN-а;
- данные достаточно стабильны, чтобы задержка обновления была приемлема.

Прямое чтение предпочтительнее, когда данные уже лежат в write-таблице в нужном виде (карточка лота, история ставок), или когда нужна строгая консистентность (страница оплаты счёта).

### EndingSoon: вычисляется на чтении, не хранится

Поле `EndingSoon` в `CatalogItem` — хороший пример того, почему проекции не должны хранить всё. Флаг «заканчивается скоро» зависит от текущего времени: через час после записи в проекцию он может стать true без каких-либо новых событий. Если хранить его в таблице, он протухает:

```go
// internal/auction/adapters/auction_pg_read_models.go

// ActiveAuctions serves the public catalog. EndingSoon is computed at
// read time (ends_at - now() < 5 min) — never stored (§5).
func (m *AuctionPostgresReadModels) ActiveAuctions(ctx context.Context, page, pageSize int) (query.CatalogPage, error) {
    rows, err := m.db.QueryContext(ctx, `
        SELECT auction_id, title, seller_id, currency,
               current_price_minor, minimal_next_bid_minor, bid_count, ends_at,
               (ends_at - now()) < interval '5 minutes' AS ending_soon
        FROM auction.catalog_items
        ORDER BY ends_at, auction_id
        LIMIT $1 OFFSET $2`, pageSize, (page-1)*pageSize)
    ...
}
```

`(ends_at - now()) < interval '5 minutes'` вычисляет Postgres в момент чтения. Результат всегда свежий, и не нужно никаких событий, триггеров или фоновых обновлений для поддержания флага в актуальном состоянии. Это важный принцип: проекции хранят данные, которые без нового события не изменятся. Производные от времени значения — считаются на лету.

---

## Трейдоффы

**CQRS без шины — это не «неполный» CQRS.** Шина команд добавляет decoupling между портом и хендлером, но вместе с ней приходят: сложность маршрутизации, потеря compile-time проверки, overhead на dispatch. В монолите прямой вызов `h.commands.PlaceBid.Handle(ctx, cmd)` ничем не хуже. Шина понадобится, когда хендлеры нужно будет запускать асинхронно или маршрутизировать на несколько потребителей — это откладываемое решение, не стартовое.

**Проекция — это долг обслуживания.** За каждую проекцию команда платит: нужно поддерживать хендлеры событий, следить за lag-ом, держать в уме eventual consistency. В Molot `participant` обходится вообще без проекций (CRUD), `notification` — без domain/app слоёв. Золотое правило 33: CQRS не применяется к тривиальному CRUD; исключение задокументировано.

**Eventual consistency требует честности.** Если между записью ставки и обновлением каталога пройдёт несколько секунд (задержка Watermill + Postgres) — это нормально для каталога. Это недопустимо для страницы оплаты, где пользователь ожидает актуальный статус счёта. Карточка аукциона в Molot читает write-таблицу именно поэтому.

**Размер read model определяет стоимость миграции.** Если завтра `CatalogItem` нужно добавить поле — достаточно изменить проекцию и хендлер события. Если бы вместо `CatalogItem` использовался доменный агрегат `Auction`, добавление поля в агрегат затрагивало бы и write-side, и все читающие.

---

## Типичные ошибки

**Ошибка 1: командный слой «пропустим для простоты».**

«Тут простая операция, сделаем прямо из хендлера». Через месяц у той же операции появляется логирование, валидация, ограничение по ролям. Каждое добавляется в HTTP-хендлер. Потом появляется gRPC-хендлер, который дублирует часть логики. Правило аудита 28: командный слой не пропускается. `app.Application` — единственная точка входа для всех портов.

**Ошибка 2: команда возвращает агрегат.**

```go
// так нельзя
func (h PlaceBidHandler) Handle(ctx context.Context, cmd PlaceBid) (*auction.Auction, error)
```

Это замораживает команду в синхронном режиме навсегда. Нельзя сделать её асинхронной без смены контракта. Нельзя поставить в очередь без изменения всех вызывающих. Нельзя кэшировать ответ. Правило: команда возвращает только `error`. Если клиенту нужны данные — он делает отдельный запрос.

**Ошибка 3: HTTP-статусы из хендлеров.**

```go
// так нельзя
func (h PlaceBidHandler) Handle(ctx context.Context, cmd PlaceBid) (int, error) {
    // возвращаем 409, 403, 404...
}
```

Хендлер — часть app-слоя, он не знает о HTTP. Статусы живут в порту. Между ними — slug-ошибки: `errs.NewConflictError("bid-below-minimum")`. Порт транслирует `ErrorKind` в статус через один общий хелпер. Это позволяет использовать один и тот же хендлер из HTTP, gRPC и worker без какого-либо изменения app-слоя.

```go
// internal/auction/app/command/place_bid.go — правильно

func mapPlaceBidError(err error) error {
    ...
    case errors.Is(err, auction.ErrBidBelowMinimum):
        return errs.NewConflictError("bid-below-minimum").WithCause(err)
    case errors.Is(err, auction.ErrVerificationRequired):
        return errs.NewForbiddenError("verification-required").WithCause(err)
    ...
}
```

`ErrorKind` (`Conflict`, `Forbidden`, `NotFound`) — абстракция, не статус. Порт решает, что с ней делать.

---

## Когда CQRS не нужен

Правило 33 аудита звучит так: CQRS и слои не применяются к тривиальному CRUD и auth; исключение задокументировано и пересматривается при росте логики.

Контекст `participant` в Molot — почти CRUD: регистрация участника, верификация. Слои есть, но проекций нет, отдельной read model нет, история ставок у него отсутствует. Это осознанное решение, а не забытое. В `notification` нет domain и app слоёв вообще: событие → шаблон → отправка. Комментарий в пакете прямо говорит: «появится логика — появятся слои».

Цена ложного применения CQRS — overhead структур и слоёв без пользы. Это та же ошибка, что и «пропустить командный слой», только в другую сторону: паттерн пропорционален сложности того, что он защищает.

Практический признак: если для реализации use case не нужна доменная модель (нет инвариантов, которые надо защитить), если результат чтения совпадает с тем, что лежит в таблице — командный слой и read model добавляют шум без сигнала.

---

## Чек-лист

- [ ] Команда — struct с доменными типами полей, не с примитивами.
- [ ] `Handle(ctx, cmd) error` — только `error`, никаких бизнес-данных.
- [ ] ID создаваемой сущности — клиент-сгенерированный; ответ — `204 + content-location`.
- [ ] Query handler возвращает UI-shaped результат: не агрегат, не DB-строку, не OpenAPI-DTO.
- [ ] Все хендлеры в `app.Application{Commands, Queries}` обёрнуты `ApplyCommandDecorators`/`ApplyQueryDecorators`.
- [ ] Порты вызывают только `app.Commands.X.Handle(...)` — никогда репозиторий напрямую.
- [ ] Slug-ошибки в app-слое; HTTP-статусы — только в портах, через один хелпер.
- [ ] Для каждого query выбор источника данных (проекция vs прямое чтение) задокументирован.
- [ ] Временно зависимые поля (`EndingSoon`) вычисляются на чтении, не хранятся в проекции.
- [ ] Для тривиального CRUD применение CQRS задокументировано как исключение с условием пересмотра.

---

## Ссылки

- ARCHITECTURE.md §3 — каталог всех команд и запросов с типами и сигнатурами.
- ARCHITECTURE.md §5 — read models: два источника, интерфейсы, схема `catalog_items` и `seller_dashboard_items`.
- BOOK_AUDIT.md правила 26–33 — формальные требования к команды/запросам, декораторам, slug-ошибкам.
- `internal/auction/app/app.go` — `Application{Commands, Queries}`, точка входа.
- `internal/auction/app/command/place_bid.go` — канонический пример командного хендлера.
- `internal/auction/app/query/active_catalog.go` — канонический пример query handler с UI-shaped результатом.
- `internal/auction/adapters/catalog_pg_projection.go` — проекция с монотонным guard-ом.
- `internal/auction/adapters/auction_pg_read_models.go` — оба пути чтения в одном адаптере.
- `internal/common/decorator/decorator.go` — `ApplyCommandDecorators`/`ApplyQueryDecorators`.
