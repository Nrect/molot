# Глава 4. Слои и направление зависимостей: Clean + Hexagonal

---

## Зачем читать

После этой главы вы будете понимать, почему Clean Architecture, Hexagonal
Architecture и Onion Architecture — это одна и та же идея в трёх разных
системах обозначений; как она реализована в Molot на реальном коде; почему
каждый слой требует собственной модели данных; где должны жить HTTP-хендлеры,
воркеры и репозитории; и сколько на самом деле стоит маппинг — честно, с
трейдоффами.

---

## Проблема: инфраструктура прорастает в бизнес

Возьмём типичный Go-сервис через год после запуска. В структурах висят теги
`db:` и `json:` одновременно. SQL-запросы рождаются в хендлерах. Сервис
возвращает HTTP-статусы из метода, который «просто проверяет баланс». Поменять
Postgres — квест на неделю, потому что `*sql.Tx` каким-то образом добрался
до use case.

Корень проблемы — отсутствие явного ответа на вопрос: **что знает о чём**.
Бизнес-правила — самый стабильный код в системе: они переживают фреймворки,
базы данных и версии API. Когда они сцеплены с инфраструктурой, самый
долгоживущий код вынужден умирать вместе с самым короткоживущим.

Реальный пример из первоисточника (BOOK_AUDIT §1): один struct на хранилище
и на транспорт привёл к утечке поля `LastIp` всем пользователям — потому что
у хранения и у HTTP-ответа разные причины меняться, и изменение схемы БД
автоматически изменило API.

---

## Теория: три названия одной идеи

**Clean Architecture** (Robert Martin), **Hexagonal Architecture** (Alistair
Cockburn, она же «Ports & Adapters»), **Onion Architecture** (Jeffrey Palermo)
— разные визуализации одного принципа: **зависимости направлены внутрь,
к домену**. Домен не знает ни о каком внешнем мире.

- Clean: концентрические кольца — Entities → Use Cases → Interface Adapters →
  Frameworks & Drivers. Стрелки — только внутрь.
- Hexagonal: шестигранник с доменом в центре, «driving ports» (то, что
  инициирует действие) и «driven ports» (то, что система вызывает).
- Onion: те же кольца, акцент на том, что Infrastructure — снаружи всего.

Суть одна: **инверсия зависимостей**. Абстракции объявляет тот, кто ими
пользуется — потребитель, а не поставщик. Хотите заменить Postgres —
пишете новый адаптер, реализующий интерфейс, объявленный в домене. Домен
и use cases не трогаете вообще.

### ports ≠ adapters

Граница, которую путают чаще всего:

- **Ports** — входящая сторона (driving): HTTP-хендлеры, event-подписчики,
  воркеры, CLI-команды. Они _вызываются_ внешним миром и _вызывают_ app-слой.
- **Adapters** — исходящая сторона (driven): репозитории, клиенты платёжных
  шлюзов, email-отправители, проекции. Их _вызывает_ app-слой через
  интерфейсы, объявленные в том же app-слое.

Порты не импортируют адаптеры — это правило проверяется линтером в CI
(`go-cleanarch`, BOOK_AUDIT §2).

---

## Как в Molot

### Раскладка слоёв

В каждом bounded context — одна и та же структура:

```
internal/auction/
    domain/auction/      # агрегат, VO, доменные события, интерфейс Repository
    app/
        command/         # команды (write use cases)
        query/           # запросы (read use cases)
    ports/               # HTTP-хендлеры, воркер, event-подписчики (driving)
    adapters/            # Postgres-репозитории, проекции, маперы (driven)
    events/              # публичные интеграционные события (импортируемые извне)
    service/             # локальная сборка: NewService(deps) → фасад
```

Правила направления — не конвенция, а CI-барьер. Импорт нарушителя падает в
пайплайне до code review.

---

### Одна сущность в трёх представлениях

Посмотрим на лот аукциона в трёх слоях — и поймём, почему у каждого своя
модель.

#### 1. Storage-модель: `pgAuction` в адаптере

Файл `internal/auction/adapters/auction_pg_repository.go`, строки 45–78:

```go
// pgAuction is the storage model — never shared with domain or
// transport (rule 7); the domain is rebuilt only through
// auction.UnmarshalFromDatabase.
type pgAuction struct {
    ID                  uuid.UUID
    SellerID            uuid.UUID
    Title               string
    Description         string
    Currency            string
    StartPriceMinor     int64
    IncrementMinor      int64
    ReservePriceMinor   sql.NullInt64
    StartsAt            time.Time
    EndsAt              time.Time
    OriginalEndsAt      time.Time
    SnipeWindowSec      int64
    SnipeExtensionSec   int64
    SnipeMaxExt         int
    VerifyAboveMinor    int64
    ExtensionsUsed      int
    Status              string
    Outcome             sql.NullString
    LeadingBidID        uuid.NullUUID
    LeadingBidderID     uuid.NullUUID
    LeadingAmountMinor  sql.NullInt64
    LeadingPlacedAt     sql.NullTime
    // ... и т.д.
    Version             int64
}
```

Заметьте: `sql.NullInt64`, `uuid.NullUUID`, `sql.NullTime` — это Postgres-
специфика. Она живёт здесь и нигде больше. Поля плоские: нет доменных типов
`Money`, `BiddingWindow`, `AntiSnipePolicy`. Причина изменения этой структуры —
изменение схемы базы данных.

#### 2. Доменный тип: `Auction` в домене

Файл `internal/auction/domain/auction/auction.go`, строки 14–36:

```go
// Auction is the central aggregate of the context (§2.1). The full bid
// history belongs to its consistency boundary but is never loaded:
// bid invariants depend only on the head of state, so the aggregate
// keeps the top two bids denormalized (ADR-0003). All fields are
// unexported (rule 8); state changes go through behavior methods that
// guard the invariant and transition atomically (rule 9).
type Auction struct {
    id               AuctionID
    seller           SellerID
    lot              Lot
    startPrice       Money
    increment        Money
    reserve          ReservePrice // zero = no reserve; hidden from events/API
    window           BiddingWindow
    antiSnipe        AntiSnipePolicy // snapshot taken at listing time
    verifyAbove      Money
    extensionsUsed   int
    status           Status
    outcome          Outcome
    leadingBid       Bid
    runnerUpBid      Bid
    winnerReassigned bool
    bidCount         int
    relistOf         AuctionID
    relistGen        int
    settled          bool
    version          int64
    events           []DomainEvent
}
```

Все поля приватные. Нет `db:`-тегов, нет `json:`-тегов, нет `uuid.UUID` —
только доменные типы: `Money`, `BiddingWindow`, `AntiSnipePolicy`. Ни одного
импорта из `database/sql` или `encoding/json`. Причина изменения этой
структуры — изменение бизнес-правил.

#### 3. Transport-DTO: `AuctionCardView` в query-слое

Файл `internal/auction/app/query/auction_card.go`, строки 16–35:

```go
type AuctionCardView struct {
    AuctionID           string
    Title               string
    Description         string
    SellerID            string
    Status              string
    Outcome             string // empty until closed
    StartPriceMinor     int64
    CurrentPriceMinor   int64
    MinimalNextBidMinor int64
    IncrementMinor      int64
    Currency            string
    HasReserve          bool // the reserve amount itself is always hidden
    StartsAt            time.Time
    EndsAt              time.Time
    ExtensionsUsed      int
    BidCount            int
    LeaderID            string // empty without bids
    RecentBids          []BidView
}
```

Здесь уже нет `sql.Null*`, нет доменных типов — есть плоские поля удобные для
HTTP-ответа. `HasReserve bool` вместо `reserve ReservePrice` — намеренное
сокрытие суммы резерва (бизнес-правило, не деталь хранения). `CurrentPriceMinor`
и `MinimalNextBidMinor` вычисляются в адаптере чтения. Причина изменения этой
структуры — изменение того, что нужно показывать клиенту.

Три структуры, три разных причины меняться. Один struct на два слоя —
не DRY, а сцепка причин для изменений. DRY относится к поведению, не к
данным (TEXTBOOK гл. 3, золотое правило 3).

---

### Маппинг явный, в адаптере

Переход между слоями — функция в адаптере, нигде больше. Из хранилища в домен:

```go
// internal/auction/adapters/auction_pg_repository.go, строки 373–459

func toDomainAuction(p pgAuction) (*auction.Auction, error) {
    id, err := auction.NewAuctionID(p.ID)
    // ...
    currency, err := auction.NewCurrency(p.Currency)
    // ...
    startPrice, err := auction.NewMoney(p.StartPriceMinor, currency)
    // ...
    window, err := auction.UnmarshalBiddingWindow(p.StartsAt, p.EndsAt, p.OriginalEndsAt)
    // ...
    return auction.UnmarshalFromDatabase(
        id, seller, lot,
        startPrice, increment, reserve,
        window, antiSnipe, verifyAbove,
        p.ExtensionsUsed, status, outcome,
        leadingBid, runnerUpBid,
        p.WinnerReassigned, p.BidCount,
        relistOf, p.RelistGeneration, p.Settled,
        p.Version,
    )
}
```

Многословно — осознанная цена. При смене схемы БД меняете только
`toDomainAuction` и `toPgAuction` — бизнес-логика не знает, что что-то
поменялось.

---

### Domain → ничего: чистота как физическое ограничение

Доменный пакет `auction` импортирует только стандартную библиотеку:

```go
// internal/auction/domain/auction/auction.go, строки 1–6

package auction

import (
    "errors"
    "time"
)
```

Это не конвенция, проверяемая на code review. Попытка добавить `database/sql`
или `github.com/google/uuid` сюда упадёт в линтере немедленно. Домен
физически не может знать о Postgres, HTTP или Watermill.

---

### App → только домен: use case как чистый оркестратор

Хендлер команды `PlaceBid` (файл `internal/auction/app/command/place_bid.go`):

```go
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

Хендлер делает ровно три вещи: достаёт профиль биддера, делегирует мутацию
через `repo.Update`, маппит ошибки домена в транспортно-нейтральные slug-ошибки.
Никакого `if bid.Amount > 0` здесь нет — это было бы протечкой бизнеса в
app-слой. Весь guard-каскад живёт в `a.PlaceBid()`.

Зависимости хендлера — интерфейсы, объявленные в том же пакете `command`
(файл `internal/auction/app/command/deps.go`):

```go
// clock is the consumer-side time source (§10): aggregates take `now`
// as a parameter, handlers obtain it here — deterministic tests, no
// sleeps.
type clock interface {
    Now() time.Time
}

// bidderProfiles reads the local projection of participant events.
type bidderProfiles interface {
    BidderByID(ctx context.Context, id auction.BidderID) (auction.Bidder, error)
}
```

Интерфейс объявляет потребитель — паттерн `io.Writer`. App-пакет не знает,
что за `BidderProfilesPostgres` стоит за `bidderProfiles`; он знает только
контракт. Адаптер реализует его неявно — никакого `implements` не нужно.
Это снимает import cycle по построению и даёт крошечные интерфейсы, для
которых тестовый шпион — буквально пара строк.

---

### Ports: driving-адаптеры — HTTP и воркер

**HTTP-хендлер** (`internal/auction/ports/http.go`, строки 25–34):

```go
// HTTPServer implements the generated StrictServerInterface on top of
// app.Application. The acting user always comes from the JWT context
// (typed auth.User) and is handed to commands as a domain type (§8).
type HTTPServer struct {
    app app.Application
}

func NewHTTPServer(application app.Application) HTTPServer {
    if application.Commands.ListAuction == nil || application.Queries.AuctionCard == nil {
        panic("NewHTTPServer: application is not fully assembled")
    }
    return HTTPServer{app: application}
}
```

`HTTPServer` знает только `app.Application`. Никаких `adapters.AuctionPostgresRepository`
здесь нет — и быть не может: `ports` не импортирует `adapters`. Задача порта —
распаковать HTTP-запрос в команду, вызвать хендлер, упаковать результат или
ошибку в HTTP-ответ.

**Воркер закрытия** — ещё один driving-адаптер, живущий в `ports/`:

```go
// internal/auction/ports/worker.go, строки 32–46

// ClosingWorker closes auctions by time (§10): tick → scan candidates
// → CloseAuction command per id. Idempotency and races (anti-snipe
// extensions, concurrent replicas) are resolved by the aggregate
// guards plus the row lock — the worker never reasons about state.
type ClosingWorker struct {
    closeAuction decorator.CommandHandler[command.CloseAuction]
    scanner      dueAuctionsScanner
    clock        workerClock
    interval     time.Duration
    logger       *slog.Logger
    // ...
}
```

Воркер — это тоже «входящий мир»: он инициирует действие (CloseAuction) так
же, как HTTP-запрос инициирует PlaceBid. Оба — в `ports/`, оба знают только
об `app.Application`. Бизнес-логика закрытия живёт в `a.Close()` в домене, не
в воркере.

---

### Adapters: driven-адаптеры — репозиторий и read models

Репозиторий (`internal/auction/adapters/auction_pg_repository.go`) — driven-
адаптер: он реализует интерфейс `auction.Repository`, объявленный в домене.
Ключевой метод `Update`:

```go
func (r *AuctionPostgresRepository) update(
    ctx context.Context,
    id auction.AuctionID,
    updateFn func(ctx context.Context, a *auction.Auction) (*auction.Auction, error),
) error {
    return postgres.RunInTx(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
        row := tx.QueryRowContext(ctx,
            `SELECT `+auctionColumns+` FROM auction.auctions WHERE id = $1 FOR UPDATE`, id.UUID())
        // ...
        a, err := toDomainAuction(p)
        // ...
        updated, err := updateFn(ctx, a)
        // ...
        // UPDATE ... WHERE id = $1 AND version = $17
        // INSERT bid history from BidPlaced events
        // publishMapped — outbox в той же транзакции
    })
}
```

Транзакция — деталь адаптера. `FOR UPDATE` — деталь адаптера. Optimistic
locking по `version` — деталь адаптера. Домен и use case не знают ни про
что из перечисленного.

Read models — тоже adapters, но driven в другом смысле: их вызывает query-
хендлер через consumer-side интерфейс `AuctionCardReadModel`:

```go
// internal/auction/app/query/auction_card.go, строки 46–48

type AuctionCardReadModel interface {
    AuctionCard(ctx context.Context, id auction.AuctionID) (AuctionCardView, error)
}
```

Реализует его `AuctionPostgresReadModels` из `adapters/`. Query-хендлер
не знает, что это Postgres.

---

### Service: composition root контекста

Файл `internal/auction/service/service.go` — единственное место, где
конкретные типы встречаются. Сборка use cases (строки 128–157):

```go
application := app.Application{
    Commands: app.Commands{
        PlaceBid: decorator.ApplyCommandDecorators(
            command.NewPlaceBidHandler(repo, profiles, clk), decorators),
        // ...
    },
    Queries: app.Queries{
        AuctionCard: decorator.ApplyQueryDecorators(
            query.NewAuctionCardHandler(readModels), decorators),
        // ...
    },
}
```

Конструкторы паникуют на nil-зависимостях — неправильная сборка падает на
старте, не на первом запросе.

---

### Когда слои не нужны: notification

Не каждый контекст требует полного стека. Контекст `notification` сознательно
обходится без `domain/` и `app/` (`internal/notification/README.md`):

> В контексте нет бизнес-инвариантов: ни состояния с переходами, ни команд,
> ни запросов. Единственная «логика» — выбор шаблона, адресата и дедупликация
> доставки; она объявлена декларативно в `ports/events.go` рядом с подписками.
> Вводить domain/app здесь — слои-пустышки (агрегат без инвариантов, app-
> хендлер из одной строки), что чек-лист запрещает так же, как и пропуск
> слоёв там, где они нужны.

Пайплайн `notification` — это `событие → шаблон → отправка`. Для него
достаточно `ports/` (подписки, driving) и `adapters/` (хранилище слотов,
driven). Consumer-side интерфейсы объявлены прямо в `ports/events.go`:

```go
// internal/notification/ports/events.go, строки 41–60

type EmailSender interface {
    Send(ctx context.Context, to, subject, body string) error
}

type RecipientDirectory interface {
    UpsertRecipient(ctx context.Context, participantID, email, displayName string) error
    RecipientByID(ctx context.Context, participantID string) (email, displayName string, err error)
}
```

README фиксирует **условие пересмотра**: как только появляются пользовательские
предпочтения каналов, digest, расписания тишины, повторные отправки — контекст
получает полноценные `domain/app` по общим правилам, и исключение удаляется.
Паттерн применяется пропорционально сложности (BOOK_AUDIT §1, правило 28/33).

---

## Трейдоффы

**Цена слоёв:**

- Маппинговый код. В крупном контексте — сотни строк `toDomainAuction` /
  `toPgAuction`. Скучные, но предсказуемые.
- Больше файлов. Один write-use-case в Molot = пять файлов (команда,
  хендлер, адаптер, порт, сервис). В CRUD-сценарии ощутимо много.

**Что вы получаете:**

- Замена Postgres — задача одного файла в `adapters/`.
- Домен тестируется unit-тестами без моков, без Docker, без сети: пакет
  `auction` импортирует только `errors` и `time`.
- Инцидент `LastIp` физически невозможен: поле появляется в `pgAuction`,
  не попадая в `AuctionCardView`.
- Декораторы (logging, RED-метрики, tracing) добавляются раз к app-слою,
  бизнес-код остаётся чистым.

**Когда слои не нужны:** CRUD без инвариантов, пайплайны с прямолинейной
трансформацией. Правило Molot: исключение документируется с условием
пересмотра.

---

## Типичные ошибки

**Бизнес-if в app-слое.** Хендлер `PlaceBid` не проверяет `amount > 0` —
это делает `a.PlaceBid()`. Если в хендлере появляется `if cmd.Amount.IsZero()` —
правило пролезло не в свой слой. BOOK_AUDIT §3: «Любой бизнес-if в app —
переезжает в домен».

**HTTP-статусы из use case.** Use case возвращает доменные sentinel-ошибки
(`ErrAuctionNotOpen`). Маппинг в статусы — один раз, в `ports/` через
`httperr.RespondWithSlugError`. Промежуток — slug-ошибки (`errs.NewConflictError`),
транспортно-нейтральные: `mapPlaceBidError` в `command/place_bid.go` собирает
их, HTTP-слой переводит slug в статус.

**Импорт `adapters` из `ports`.** Порт вызывает репозиторий напрямую —
бизнес-логика обходит декораторы и транзакционный контекст. `ports` знает
только `app.Application`.

**Один struct на хранилище и транспорт.** Инцидент `LastIp` в ожидании:
поле в БД → автоматически в API-ответ. Каждая причина изменения тянет все
остальные.

**Repository с бизнес-логикой.** Репозиторий проверяет статус — логика
расщеплена, in-memory и Postgres реализации расходятся. Repository глуп:
загрузить → замапить → сохранить (BOOK_AUDIT §4).

---

## Чек-лист

Перед merge проверьте:

- [ ] `domain/` импортирует только `stdlib` — нет `database/sql`, нет UUID-
      пакетов, нет Watermill.
- [ ] `app/` импортирует только `domain/` и `common/` — нет `adapters/`,
      нет `ports/`.
- [ ] `ports/` не импортирует `adapters/` — только `app.Application`.
- [ ] Каждый слой имеет собственные структуры данных: `pgModel` /
      `DomainType` / `TransportDTO` раздельно.
- [ ] Маппинг между слоями — в адаптере, явный, с ошибкой при невалидных
      данных из БД.
- [ ] Consumer-side интерфейсы объявлены в пакете-потребителе, не в пакете-
      реализаторе.
- [ ] Конструкторы паникуют на nil-зависимостях.
- [ ] Бизнес-if в хендлере отсутствует — только вызов доменного метода.
- [ ] HTTP-статусы не возвращаются из use case — только slug-ошибки.
- [ ] Исключение (без domain/app) задокументировано с условием пересмотра.

---

## Ссылки

- **BOOK_AUDIT.md §1** — сводная таблица принципов, кейс `LastIp`
- **BOOK_AUDIT.md §2** — раскладка репозитория, правила направления, `go-cleanarch`
- **BOOK_AUDIT.md §3** — чистота домена, формальный признак протечки бизнес-if
- **BOOK_AUDIT.md §4** — Repository: интерфейс в домене, updateFn, «Repository глуп»
- **BOOK_AUDIT.md §5–7** — CQRS, consumer-side интерфейсы, события + outbox
- **TEXTBOOK.md гл. 3** — золотые правила 3 и 5; кейс `LastIp`; трейдофф «лишние структуры»
