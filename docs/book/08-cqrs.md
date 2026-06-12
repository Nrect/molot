# 8. CQRS: разделение моделей, а не инфраструктуры

## Зачем читать

С CQRS случилась несправедливость: мало какой паттерн отпугнул столько команд одним своим именем. Спросите коллегу, что это такое, — и с хорошей вероятностью услышите про отдельную шину, отдельное хранилище и event sourcing в придачу. Неудивительно, что большинство либо игнорирует идею целиком, либо откладывает её «до зрелости» — а зря: между «ничего» и «всё сразу» есть практичная середина, которую Molot использует с первого дня без единого дополнительного брокера и без event sourcing. В этой главе разберёмся, чем CQRS является на самом деле, где он окупается, а где только шумит, — и как распознать три ошибки, которые убивают идею на корню.

---

## Проблема

Начнём с кода, который выглядит совершенно безобидно — такой метод есть почти в каждом проекте:

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

Присмотритесь: этот метод делает сразу три вещи, и у каждой — свои требования. Как хранилище данных он хочет нормализации и инвариантов. Как источник ответа для HTTP — плоской структуры под конкретный экран. Как транзакционная единица — минимального объёма данных под локом. Пока проект молод, конфликт незаметен. Потом он начинает собирать дань.

Сначала появляются методы с флагами: `GetAuction(ctx, id, withBids bool, withSeller bool)` — каждый флаг означает «ещё одному экрану понадобилось ещё одно подмножество полей». Потом выясняется, что чтения нельзя кэшировать: они намертво сцеплены с мутирующей логикой. Любая оптимизация чтения рискует сломать запись, и наоборот. А тесты на логику ставки вынуждены собирать «полный» объект, хотя им нужна горстка полей. Если это звучит знакомо — дальше будет легче: у этой боли есть имя и недорогое лекарство.

---

## Что такое CQRS (и чем он не является)

Command Query Responsibility Segregation в минимальном определении — это **две модели вместо одной**: отдельная модель для записи (Command side) и отдельная для чтения (Query side). Всё, точка. Остальное — необязательные надстройки, и стоит проговорить вслух, какие именно:

- не event sourcing (CQRS работает с обычными реляционными таблицами);
- не обязательная шина команд (прямой вызов хендлера — полноценный CQRS);
- не отдельная база данных для чтений (read model можно строить на тех же таблицах);
- не eventual consistency по умолчанию (прямое чтение write-таблиц — строго консистентно).

Если нужна бытовая аналогия — это бухгалтерия. Бухгалтер ведёт журнал проводок по строгим правилам: двойная запись, никаких исправлений задним числом. А директору на стол кладёт одностраничную сводку, собранную из журнала под конкретный вопрос. Никому не приходит в голову требовать, чтобы директор читал сам журнал, — или чтобы сводка соблюдала правила двойной записи. Две формы одних и тех же данных, у каждой свой потребитель и свои законы. Вот и весь CQRS.

Суть — в разделении ответственностей, а не в конкретной инфраструктуре. Это важно проговорить, потому что многие команды откладывают CQRS до момента, когда «созреют» для шины и отдельного хранилища, — хотя основную пользу, упрощение кода и развязку эволюции чтений и записей, можно забрать немедленно и бесплатно.

---

## Как в Molot

### Единый каталог: `app.Application{Commands, Queries}`

Первое, что стоит увидеть в любом контексте Molot, — каталог его use cases. Граница между write и read side проходит прямо по типам:

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

`Application` собирается один раз в `service/NewService` и инжектируется во все порты, и порты вызывают только хендлеры из этого каталога — никогда репозиторий или адаптер напрямую. Обратите внимание на побочный эффект, который со временем оказывается дороже основного: `Commands`/`Queries` — это оглавление контекста. Новый человек открывает `app.go` и за тридцать секунд видит всё, что контекст умеет: восемь команд, четыре запроса, ни одной скрытой двери. Ни одна wiki не даст такой гарантированно свежей карты.

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

Смотрите на типы полей: `AuctionID` — не `string`, не `uuid.UUID`, а `auction.AuctionID`. Это не педантизм. Команда с четырьмя идентификаторами — идеальное место, чтобы перепутать их при вызове, и такая опечатка тихо доживает до продакшена. С доменными типами «передал `BidID` туда, где ждут `AuctionID`» просто не скомпилируется. Дешевле страховки не бывает.

Теперь сам хендлер:

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

Первое, что бросается в глаза: `Handle` возвращает только `error`. Никаких бизнес-данных — и это не упущение, а контракт с дальним прицелом. Если завтра `PlaceBid` понадобится сделать асинхронным — унести в очередь, в воркер — сигнатура не изменится ни на символ, а значит, не изменится ни один вызывающий. HTTP-порт уже сейчас живёт по этим правилам: отвечает `204 + content-location`, и клиент знает, где прочитать результат, не требуя его от команды.

То же правило выдерживает и `ListAuction` — команда, создающая сущность, то есть случай, где соблазн вернуть данные сильнее всего: «как же клиент узнает ID?» Ответ: клиент его уже знает, потому что сам сгенерировал:

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

Клиент-сгенерированный UUID делает `Add` идемпотентным: повторный POST с тем же ID — тихий no-op. Серверу не нужно ни генерировать идентификаторы, ни возвращать их обратно. Паттерн «client-generated UUID + 204» решает проблему дублей при ретраях бесплатно — без таблиц дедупликации и специальных заголовков.

**Совет из практики.** Отдавать генерацию ID клиенту страшно ровно до первого инцидента с ретраями. Мобильный клиент на плохой сети повторяет POST, потому что не дождался ответа, — и при серверной генерации ID вы получаете два лота вместо одного. Дальше начинается знакомая спираль: таблица дедупликации, заголовок `Idempotency-Key`, чистка по TTL. Клиентский UUID закрывает всё это одним решением. Единственное, что остаётся серверу, — валидировать, что пришёл действительно UUID, а не произвольная строка.

### Запрос: UI-shaped результат, не доменный объект

Теперь вторая сторона границы. Правило здесь такое же жёсткое, только смотрит в другую сторону: query handler никогда не возвращает доменный агрегат. Он возвращает структуру, скроенную под конкретный экран.

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

Присмотритесь к тому, чего здесь *нет*: ни reserve, ни antisnipe policy, ни деталей extension-ов — каталогу они не нужны, и наружу они не едут. Зато есть `MinimalNextBidMinor` — поле, которого нет в агрегате: каталог хочет показывать готовую цифру, а не заставлять фронтенд складывать цену с инкрементом. `CatalogItem` — не `Auction`, не OpenAPI-DTO из `openapi.gen.go`, не DB-строка. Да, это ещё одна структура. И это нормально.

Правило из аудита 26: результат query — UI-shaped, не домен, не OpenAPI, не DB-модель. Это требует трёх отдельных структур, и рука регулярно тянется их «схлопнуть». Не поддавайтесь: вместе с главой 3 (DRY относится к поведению, а не к данным) здесь работает один и тот же аргумент — «лишние» структуры не overhead, а плата за то, что каждый слой меняется по своей причине и в своём ритме.

### Generic-декораторы: один раз для всего

Внимательный читатель уже заподозрил подвох: двенадцать хендлеров, и в каждом нужны логирование, метрики и трейсинг? Копировать `logger.Info(...)`, `metrics.Record(...)`, `span.Start(...)` в каждый `Handle` — верный способ получить наблюдаемость «местами»: где-то есть, где-то забыли. В Molot для этого есть единая точка навески:

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

Читайте цепочку изнутри наружу: metrics — внутренний слой, измеряет чистое время хендлера; над ним logging; самый внешний — tracing, чтобы и логи, и метрики записывались уже внутри span context. Имя use case извлекается рефлексией из типа команды — `PlaceBid` становится `"commands/PlaceBid"` в трейсе, и никто не поддерживает этот реестр руками. Каждый хендлер каталога оборачивается один раз при сборке в `service/NewService`: новый хендлер — одна строка с `ApplyCommandDecorators`, новый cross-cutting concern — один декоратор для всех сразу.

Результат стоит проговорить отдельно: в бизнес-коде хендлера (`PlaceBidHandler.Handle`) нет ни строчки логирования, метрик или трейсинга — только дело. Золотое правило 12: cross-cutting живёт в декораторах, не в хендлерах.

### Read models: два честных пути в одном проекте

Остался вопрос, который чаще всего всплывает на ревью: откуда query handler берёт данные? Ответ Molot прагматичен: откуда удобнее — это решается на уровне адаптера, прозрачно для хендлера, и в проекте сосуществуют два честных пути:

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

Ключ к этому запросу — `WHERE ci.bid_count < $3`, монотонный guard: и повторная доставка события, и обновление, пришедшее не по порядку, становятся no-op. Без него redelivery могло бы откатить цену в каталоге назад — и покупатель увидел бы, как ставка «дешевеет». К идемпотентности мы вернёмся всерьёз в главе 9; пока запомните сам приём: проекция движется только вперёд. Подходят проекции туда, где читают много, а небольшая задержка обновления никому не мешает.

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

Никакой магии: обычный SELECT из write-таблицы — строго консистентный, без отдельной таблицы и без обработчиков событий, которые надо сопровождать. Заметьте деталь в комментарии: резервная цена не покидает адаптер, наружу уходит только факт `HasReserve`. А `BidHistory` устроен ещё проще: `auction_bids` — append-only таблица, она и есть история в готовом виде, проецировать нечего.

**Как выбирать между двумя путями.** Правило аудита 32 задаёт точку отсчёта: read и write стартуют на одной БД, и не всякому query нужен read model. Проекция оправдана, когда:
- запрос агрегирует данные из нескольких источников (каталог собирает поля из событий разных типов);
- нагрузка на чтение такова, что отдельный индекс или предагрегация заметно дешевле JOIN-а;
- данные достаточно стабильны, чтобы задержка обновления была приемлема.

Во всех остальных случаях прямое чтение проще и честнее: данные уже лежат в write-таблице в нужном виде (карточка лота, история ставок) — или нужна строгая консистентность (страница оплаты счёта), которой проекция не даст по определению.

**Нюанс.** Самая частая жалоба на eventually consistent проекцию звучит так: «поставил ставку, обновил страницу — каталог показывает старую цену». Это не баг проекции, это её определение. Честных решения два: либо экран, который пользователь видит сразу после своей команды, читает строго консистентный источник (как карточка лота в Molot), либо UI оптимистично показывает то, что только что отправил. Чего делать нельзя — «чинить» проекцию синхронным обновлением из командного хендлера: получите сцепку записи с чтением, ради устранения которой всё и затевалось.

### EndingSoon: вычисляется на чтении, не хранится

И прежде чем закрыть тему проекций — один маленький, но показательный случай. Поле `EndingSoon` в `CatalogItem` зависит от текущего времени: лот, записанный в проекцию утром, к вечеру становится «заканчивается скоро» без единого нового события. Положите такой флаг в таблицу — и он начнёт протухать между событиями:

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

`(ends_at - now()) < interval '5 minutes'` вычисляет Postgres в момент чтения — результат всегда свежий, и не нужно ни событий, ни триггеров, ни фоновых джобов для поддержания флага. Принцип стоит запомнить дословно: проекция хранит то, что без нового события не изменится; всё производное от времени считается на лету.

**Где вы на это наступите.** Симптом хранимого время-зависимого поля всегда один: «бейдж „ending soon“ то не появляется вовремя, то висит на лоте, который закончился час назад». Типичная «починка» — фоновый джоб, раз в минуту пересчитывающий флаги. Теперь у вас есть джоб, который надо мониторить, который отстаёт под нагрузкой и дерётся за строки с обработчиком проекции. Один `now()` в SELECT-е убирает весь этот зверинец.

---

## Трейдоффы

Как и любое архитектурное решение, разделение моделей — сделка. Вот её условия, без рекламы.

**CQRS без шины — это не «неполный» CQRS.** Шина команд добавляет decoupling между портом и хендлером, но привозит с собой сложность маршрутизации, потерю compile-time проверки и overhead на dispatch. В монолите прямой вызов `h.commands.PlaceBid.Handle(ctx, cmd)` ничем не хуже — а отлаживается заметно приятнее. Шина понадобится, когда хендлеры придётся запускать асинхронно или маршрутизировать на несколько потребителей. Это откладываемое решение, не стартовое — вот и отложите его.

**Проекция — это долг обслуживания.** За каждую проекцию команда платит постоянную ренту: хендлеры событий надо поддерживать, lag — мониторить, eventual consistency — держать в голове при каждом изменении UI. Поэтому в Molot `participant` обходится вообще без проекций (это CRUD), а `notification` — без domain/app слоёв. Золотое правило 33: CQRS не применяется к тривиальному CRUD; исключение задокументировано.

**Eventual consistency требует честности.** Несколько секунд между записью ставки и обновлением каталога (задержка Watermill + Postgres) — нормально для каталога и недопустимо для страницы оплаты, где пользователь ждёт актуальный статус счёта. Карточка аукциона в Molot читает write-таблицу именно поэтому: задержка там стоила бы доверия пользователя, а не миллисекунд.

**Размер read model определяет стоимость миграции.** Понадобилось новое поле в `CatalogItem` — меняете проекцию и хендлер события, write side об этом даже не узнаёт. Если бы каталог возил доменный агрегат `Auction`, то же поле пришлось бы протаскивать через агрегат — задев и write side, и всех остальных читающих.

---

## Типичные ошибки

**Ошибка 1: командный слой «пропустим для простоты».**

Звучит всегда одинаково: «тут простая операция, сделаем прямо из HTTP-хендлера». Через месяц у той же операции появляются логирование, валидация, проверка ролей — и всё это оседает в HTTP-хендлере. Потом появляется gRPC-порт, и логика дублируется уже туда. Дешевле всего пресечь это в нулевой день. Правило аудита 28: командный слой не пропускается; `app.Application` — единственная точка входа для всех портов.

**Ошибка 2: команда возвращает агрегат.**

```go
// так нельзя
func (h PlaceBidHandler) Handle(ctx context.Context, cmd PlaceBid) (*auction.Auction, error)
```

Одной сигнатурой команда заморожена в синхронном режиме навсегда: не сделать асинхронной без смены контракта, не поставить в очередь без правки всех вызывающих, не закэшировать ответ. Правило простое: команда возвращает только `error`, а если клиенту нужны данные — он делает отдельный запрос. Запрос для этого и существует.

**Ошибка 3: HTTP-статусы из хендлеров.**

```go
// так нельзя
func (h PlaceBidHandler) Handle(ctx context.Context, cmd PlaceBid) (int, error) {
    // возвращаем 409, 403, 404...
}
```

Хендлер — часть app-слоя; о HTTP он не знает и знать не должен. Статусы живут в порту, а между ними ходят slug-ошибки вида `errs.NewConflictError("bid-below-minimum")`: порт транслирует `ErrorKind` в статус через один общий хелпер. Награда за эту дисциплину — один и тот же хендлер обслуживает HTTP, gRPC и worker без единой правки app-слоя.

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

Обратите внимание: `ErrorKind` (`Conflict`, `Forbidden`, `NotFound`) — абстракция намерения, не статус. Что с ней делать — 409 в HTTP или соответствующий код в gRPC — решает порт, и только он.

---

## Когда CQRS не нужен

Хорошая глава о паттерне обязана честно сказать, где паттерн вреден. Правило 33 аудита формулирует это прямо: CQRS и слои не применяются к тривиальному CRUD и auth; исключение задокументировано и пересматривается при росте логики.

Посмотрите, как это выглядит вживую. Контекст `participant` в Molot — почти CRUD: регистрация участника, верификация. Слои есть, но ни проекций, ни отдельной read model, ни истории ставок — и это решение принято, а не забыто. В `notification` нет domain и app слоёв вообще: событие → шаблон → отправка. Комментарий в пакете прямо говорит: «появится логика — появятся слои».

Цена ложного применения CQRS — структуры и слои без пользы, шум без сигнала. Это та же ошибка, что «пропустить командный слой», только вывернутая наизнанку: паттерн должен быть пропорционален сложности того, что он защищает.

Практический признак, по которому легко себя проверить: если для use case не нужна доменная модель — нет инвариантов, которые надо защищать, — а результат чтения совпадает с тем, что лежит в таблице, то командный слой и read model вам сейчас не нужны. Зафиксируйте исключение письменно и возвращайтесь к нему, когда логика подрастёт.

---

## Чек-лист

Перед тем как закрыть главу, прогоните свой контекст по списку:

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
