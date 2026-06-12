# Глава 18. Каталог антипаттернов

## Зачем читать

Хороший дизайн легче описывать от обратного: не «делай вот так», а «видишь вот это в коде — остановись». Эта глава — систематический каталог ошибок, которые появляются не от незнания Go, а от незнания того, чем кончаются привычные shortcuts через полгода работы в production.

Каждый раздел устроен одинаково: название антипаттерна, как он выглядит в коде (помечено **— антипример**), чем это заканчивается конкретно, и как сделано в Molot с указанием файла. В конце — сводная таблица «запах → глава».

Антипаттерны сгруппированы по природе боли: сначала доменные (где ломается модель), затем инфраструктурные (где ломается граница слоёв), затем архитектурные (где ломается система в целом), затем тестовые и процессные.

---

## Группа 1. «Java в Golang»

**Как выглядит.** Разработчик переносит привычки из Java/C# 1:1: каждый «сервис» — это struct с одним методом на use case, статeless «domain service object» вместо простой функции, интерфейс на одну реализацию ради «расширяемости», `CalculatePriceService.Calculate(...)` вместо `func CalculatePrice(a Auction) Money`.

```go
// — антипример
type BidValidatorService struct{}

func (s BidValidatorService) Validate(a *Auction, amount Money) error {
    // логика, которая — просто функция
}
```

**Чем кончается через полгода.** Код приобретает слой «сервисов», которые не несут состояния, но при этом нужно мокировать. Их интерфейсы растут до десятков методов. Тесты тестируют моки. «Расширяемость» так и не понадобилась, а сложность осталась.

**Как правильно.** Stateless-вычисления — простые функции в доменном пакете. В Molot: `MinimalNextBid` и `CanSellerManageAuction` — пакетные функции без receiver-а, вызываются прямо из behavior-метода агрегата.

```go
// internal/auction/domain/auction/auction.go

// MinimalNextBid — пакетная функция, не «сервис»
func MinimalNextBid(a Auction) Money {
    if a.leadingBid.IsZero() {
        return a.startPrice
    }
    minNext, err := a.leadingBid.Amount().Add(a.increment)
    if err != nil {
        panic("auction: leading bid and increment currency diverged: " + err.Error())
    }
    return minNext
}

func CanSellerManageAuction(s SellerID, a Auction) error {
    if s != a.seller {
        return ForbiddenAuctionManagementError{Actor: s, Owner: a.seller}
    }
    return nil
}
```

Когда нужно разделить расчёт и мутацию — два отдельных вызова, не один «сервис». Receiver добавляется только при переходе состояния. (Глава 4.)

---

## Группа 2. Anemic model

**Как выглядит.** Доменные типы — data bags: публичные поля или тривиальные геттеры/сеттеры. Бизнес-правила живут в хендлерах. Состояние кодируется булевыми парами или указателями (`*string` как «опциональное»). Роли — magic strings.

```go
// — антипример
type Auction struct {
    Status    string
    LeaderID  *string   // nil = нет ставок
    IsClosed  bool
    IsCancelled bool
}

// в хендлере:
if a.Status == "listed" && !a.IsClosed && a.LeaderID == nil {
    a.Status = "cancelled"
    a.IsCancelled = true
}
```

**Чем кончается.** Правило «можно отменить только аукцион без ставок» продублировано в каждом хендлере, который может захотеть отменить. Один хендлер проверил, другой — забыл. Невалидное состояние (`IsClosed=true` при `Status="listed"`) уходит в базу. Тесты не могут сломать то, чего не существует в одном месте.

**Как правильно.** Все поля приватные. Создание — только через валидирующий конструктор. Переходы — только через behavior-методы, где guard и изменение атомарны. Перечислимые состояния — отдельные value-object типы, не строки.

```go
// internal/auction/domain/auction/status.go

type Status struct{ s string }

var (
    StatusListed    = Status{"listed"}
    StatusCancelled = Status{"cancelled"}
    StatusClosed    = Status{"closed"}
)

func NewStatusFromString(s string) (Status, error) { ... }
func (s Status) IsZero() bool   { return s == Status{} }

// internal/auction/domain/auction/auction.go — Cancel атомарно проверяет и меняет
func (a *Auction) Cancel(by SellerID, now time.Time) error {
    if err := CanSellerManageAuction(by, *a); err != nil {
        return err
    }
    // ...
    if a.HasBids() {
        return ErrAuctionHasBids
    }
    a.status = StatusCancelled
    a.record(AuctionCancelled{OccurredAt: now})
    return nil
}
```

(Глава 4.)

---

## Группа 3. Eight-thousanders — god-хендлеры

**Как выглядит.** Один HTTP-хендлер или «сервис» на 500+ строк. CRUD-эндпоинт `UpdateAuction(status *string, isClosed *bool, leaderID *string)`, который делает разные вещи в зависимости от того, какие поля не nil. «Стена if-ов» вместо named behavior.

```go
// — антипример
func (h *AuctionHandler) Update(w http.ResponseWriter, r *http.Request) {
    // разбираем все возможные поля
    // if req.Status == "cancelled" { ... }
    // if req.Status == "closed" && req.WinnerID != nil { ... }
    // if req.IsFailed { ... }
    // 400 строк ...
}
```

**Чем кончается.** Добавить новое бизнес-правило — значит вписать if в середину хендлера и надеяться, что не сломал существующие ветки. Тестировать можно только через HTTP. Corner cases не покрыты, потому что тест надо поднять весь сервер. Через полгода никто не знает, какие комбинации входов допустимы.

**Как правильно.** Один use case — одна named команда с `Handle(ctx, cmd) error`. Каждое поведение имеет имя в языке бизнеса. В Molot: `PlaceBid`, `CancelAuction`, `CloseAuction`, `AwardToRunnerUp`, `FailSale` — пять отдельных хендлеров, каждый в своём файле, каждый тестируется изолированно.

```go
// internal/auction/app/command/place_bid.go
type PlaceBid struct {
    AuctionID auction.AuctionID
    BidID     auction.BidID
    Bidder    auction.BidderID
    Amount    auction.Money
}

func (h PlaceBidHandler) Handle(ctx context.Context, cmd PlaceBid) error {
    bidder, err := h.profiles.BidderByID(ctx, cmd.Bidder)
    if err != nil {
        return err
    }
    return h.repo.Update(ctx, cmd.AuctionID, auction.ActorFromBidder(cmd.Bidder),
        func(ctx context.Context, a *auction.Auction) (*auction.Auction, error) {
            if _, err := a.PlaceBid(cmd.BidID, bidder, cmd.Amount, h.clock.Now()); err != nil {
                return nil, err
            }
            return a, nil
        })
}
```

(Глава 7.)

---

## Группа 4. DB-теги на домене / один struct на всё

**Как выглядит.** Доменный тип несёт `db:`, `json:`, `bson:`-теги. Одна структура — и хранилище, и API-ответ, и доменный объект. «Починка» — обнуление поля перед сериализацией (`user.LastIP = nil`).

```go
// — антипример
type Auction struct {
    ID       string    `db:"id"   json:"id"`
    Status   string    `db:"status" json:"status"`
    LeaderID *string   `db:"leader_id" json:"leader_id,omitempty"`
    // ... и это же отдаётся в API, и это же читается из БД
}
```

**Чем кончается.** Реальный инцидент из первоисточника: один struct на DB и API привёл к утечке поля `LastIP` всем пользователям — потому что разработчик добавил поле в БД, не подумав, что оно автоматически появится в JSON-ответе. Добавить поле в хранилище без его появления в API стало невозможно без «заплаток».

**Как правильно.** Три отдельные модели: storage struct (с `db:`-тегами, в адаптере), domain type (без тегов, в `domain/`), transport DTO (для API, в `ports/`). Маппинг явный, в адаптере. В Molot:

```go
// internal/auction/adapters/auction_pg_repository.go

// pgAuction — storage model, живёт только в adapters/
type pgAuction struct {
    ID              uuid.UUID
    Status          string
    LeadingBidID    uuid.NullUUID
    LeadingAmountMinor sql.NullInt64
    Version         int64
    // ... db-поля
}

// доменный тип auction.Auction — без единого тега, в domain/
// transport DTO — отдельно, в ports/
```

(Глава 3, глава 5.)

---

## Группа 5. DRY на данных

**Как выглядит.** «Зачем два struct, если поля одинаковые?» — и общий тип используется для хранения, API и внутренней бизнес-логики одновременно. Изменение API-формата ломает хранилище, изменение схемы БД меняет API-контракт.

```go
// — антипример
// один Bid используется везде: и в базе, и в API, и в домене
type Bid struct {
    ID        string  `db:"id" json:"id"`
    Amount    float64 `db:"amount" json:"amount"` // float64 для денег — отдельная боль
    BidderID  string  `db:"bidder_id" json:"bidder_id"`
}
```

**Чем кончается.** Потребители меняются по разным причинам: API меняется под требования клиента, БД оптимизируется под запросы, домен меняется под бизнес-правила. Общий тип означает, что любое изменение затрагивает сразу три причины. Появляются «адаптеры-заплатки» (`omitempty`, `json:"-"`), которые маскируют проблему. Поле добавили в таблицу — оно появилось в API. Поле убрали из API — сломался маппинг из БД.

**Как правильно.** DRY применяется к поведению, не к структурам данных. Три слоя — три модели, маппинг явный. Небольшое дублирование — осознанный трейдофф ради независимости слоёв.

```go
// domain: Money — value object с инвариантами
// adapters: хранит int64 minor units в pgAuction
// ports: отдаёт в JSON как { "amount": 10050, "currency": "RUB" }
// — три разных представления одного факта
```

(Глава 3.)

---

## Группа 6. Транзакции через context / неправильный updateFn

**Как выглядит.** Транзакция открывается в сервисе и кладётся в `context.Context`. Или: `*sql.Tx` прокидывается через параметры через пять слоёв. Или: `updateFn` мутирует переданный агрегат и возвращает тот же указатель, вместо того чтобы возвращать значение. Или: забыт `SELECT ... FOR UPDATE`.

```go
// — антипример
func (s *AuctionService) PlaceBid(ctx context.Context, id string, amount int) error {
    tx, _ := s.db.BeginTx(ctx, nil)
    ctx = context.WithValue(ctx, "tx", tx) // магия через контекст
    a, _ := s.repo.Get(ctx, id)            // репо тихо достаёт tx
    a.Status = "bidded"                    // прямая мутация
    s.repo.Save(ctx, a)
    tx.Commit()
    return nil
}
```

**Чем кончается.** Транзакция через context невидима в сигнатурах: добавить новый код-путь, который не участвует в транзакции, тривиально и незаметно. `FOR UPDATE` без него означает, что два одновременных запроса читают одно значение, оба «выигрывают» ставку — молча. Мутация переданного указателя без возврата означает, что адаптер не знает, что именно нужно персистировать.

**Как правильно.** Транзакция — деталь адаптера. `updateFn` возвращает новое значение. `FOR UPDATE` — в реализации `Update`. В Molot:

```go
// internal/common/postgres/postgres.go

func RunInTx(ctx context.Context, db *sql.DB,
    fn func(ctx context.Context, tx *sql.Tx) error) (err error) {
    tx, err := db.BeginTx(ctx, nil)
    if err != nil {
        return fmt.Errorf("begin tx: %w", err)
    }
    defer func() { err = FinishTransaction(err, tx) }()
    return fn(ctx, tx)
}

// FinishTransaction комбинирует rollback-ошибку с исходной — ни одна не теряется
func FinishTransaction(err error, tx *sql.Tx) error {
    if err != nil {
        if rollbackErr := tx.Rollback(); rollbackErr != nil {
            return multierr.Combine(err, fmt.Errorf("rollback tx: %w", rollbackErr))
        }
        return err
    }
    if commitErr := tx.Commit(); commitErr != nil {
        return fmt.Errorf("commit tx: %w", commitErr)
    }
    return nil
}
```

```go
// internal/auction/adapters/auction_pg_repository.go — SELECT ... FOR UPDATE
row := tx.QueryRowContext(ctx,
    `SELECT `+auctionColumns+` FROM auction.auctions WHERE id = $1 FOR UPDATE`, id.UUID())
```

(Глава 5.)

---

## Группа 7. Per-use-case методы на репозитории

**Как выглядит.** Репозиторий знает про бизнес: `ApproveRescheduleTraining(ctx, id, userID)`, `CancelAuctionIfNoBids(ctx, id)`. Или: логика валидации в теле метода репозитория. Или: `sql.ErrNoRows` возвращается как ошибка в GetOrCreate-сценарии.

```go
// — антипример
type AuctionRepository interface {
    GetOpenAuctionsForSeller(ctx context.Context, sellerID string) ([]Auction, error)
    CancelIfNoBids(ctx context.Context, id string) error
    PlaceBidAndNotify(ctx context.Context, id, bidderID string, amount int) error
}
```

**Чем кончается.** Репозиторий становится вторым app-слоем: бизнес-правила расползаются в оба места, тестировать нужно оба, замена реализации (in-memory → Postgres) требует дублировать всю логику. Интерфейс растёт вместе со списком use cases — и in-memory реализация отстаёт.

**Как правильно.** Репозиторий глуп: `Add / Get / Update`. Вся логика — в доменном агрегате внутри `updateFn`. In-memory реализация — полный заменитель Postgres.

```go
// internal/auction/domain/auction/repository.go

type Repository interface {
    Add(ctx context.Context, a *Auction) error
    Get(ctx context.Context, id AuctionID) (*Auction, error)
    Update(ctx context.Context, id AuctionID, actor Actor,
        updateFn func(ctx context.Context, a *Auction) (*Auction, error)) error
    UpdateAsSystem(ctx context.Context, id AuctionID,
        updateFn func(ctx context.Context, a *Auction) (*Auction, error)) error
}
```

(Глава 5.)

---

## Группа 8. Guard-if авторизация по хендлерам

**Как выглядит.** В каждом хендлере ручной `if user.Role != "admin" { return 403 }`. Авторизация — дублированный if, а не системное свойство. Системные фоновые потоки используют fake user с нужными правами. Identity берётся из `context.Value(...)` внутри репозитория.

```go
// — антипример
func (h *Handler) CancelAuction(w http.ResponseWriter, r *http.Request) {
    user := r.Context().Value("user").(User)
    auction, _ := h.repo.Get(r.Context(), id)
    if auction.SellerID != user.ID { // забыл здесь → privilege escalation
        http.Error(w, "forbidden", 403)
        return
    }
    // ...
}
```

**Чем кончается.** Harbor CVE: разработчик добавил новый endpoint, forgot проверить права — эскалация привилегий. «Забыть проверить» — это не человеческая ошибка, это системная проблема дизайна, при котором правило хранится в голове, а не в компиляторе.

**Как правильно.** Действующий пользователь — явный типизированный параметр. Авторизационное правило — чистая доменная функция. Вызов правила — внутри адаптера, мимо которого нет код-пути.

```go
// internal/auction/domain/auction/repository.go
Update(ctx context.Context, id AuctionID, actor Actor,
    updateFn func(ctx context.Context, a *Auction) (*Auction, error)) error

// actor — обязательный параметр; компилятор не позволит забыть
// CanSellerManageAuction вызывается внутри updateFn или до неё
```

Для системных потоков — отдельный метод с говорящим именем `UpdateAsSystem`, а не `Update` с `actor = fakeAdminUser`. (Глава 13.)

---

## Группа 9. Бизнес-if в app-слое

**Как выглядит.** Хендлер use case содержит бизнес-ветвление: `if amount > 10000 && !bidder.IsVerified()` прямо в `Handle`. HTTP-статус возвращается из use case: `return 409, ErrBidBelowMinimum`. App-пакет импортирует адаптеры напрямую.

```go
// — антипример
func (h PlaceBidHandler) Handle(ctx context.Context, cmd PlaceBid) (int, error) {
    if cmd.Amount > 10000 && !cmd.BidderVerified {
        return 403, errors.New("verification required") // статус из домена — неправильно
    }
    // ...
}
```

**Чем кончается.** Бизнес-правило «ставки выше порога требуют верификации» дублировано: раз в хендлере, раз (возможно) в домене, возможно ещё в тесте. Порог меняется в одном месте, в другом забыт. HTTP-статус из use case означает, что use case знает о HTTP — при появлении gRPC-порта придётся переписывать.

**Как правильно.** Бизнес-`if` замечен в app-слое — это формальный признак протечки: он переезжает в домен. App-слой — чистая оркестрация. Ошибки — ports-agnostic slugs, транслируются в статусы в портах.

```go
// internal/auction/domain/auction/auction.go — правило в домене
if !b.Verified() {
    needsVerification, err := amount.GTE(a.verifyAbove)
    if err != nil || needsVerification {
        return Bid{}, ErrVerificationRequired
    }
}

// internal/auction/app/command/place_bid.go — app только оркестрирует
func (h PlaceBidHandler) Handle(ctx context.Context, cmd PlaceBid) error {
    // ... никакого бизнес-if; вся логика внутри a.PlaceBid(...)
}

// internal/auction/app/command/place_bid.go — маппинг ошибок в слаги
case errors.Is(err, auction.ErrVerificationRequired):
    return errs.NewForbiddenError("verification-required").WithCause(err)
```

(Глава 4, глава 7.)

---

## Группа 10. Микросервисы как лекарство от сложности

**Как выглядит.** «Монолит стал большим — режем на микросервисы». Нарезка по существительным (UserService, AuctionService) или по экранам. Каждая бизнес-фича требует изменений в трёх сервисах и их синхронного деплоя. «Kubernetes решит проблемы дизайна».

**Чем кончается.** Distributed monolith: та же связность, что была, плюс сетевые вызовы, плюс версионирование API между командами, плюс distributed transactions (или их отсутствие с потерей консистентности), плюс зоопарк инфраструктуры. Худшее из обоих миров. DORA-исследования показывают: loose coupling достигается хорошим дизайном, а не микросервисами.

**Как правильно.** Сначала — правильные границы. Границы из доменного анализа, строгие (`internal/` + lint в CI), с общением только через события и фасады. Когда конкретный контекст фактически требует независимого деплоя или масштаба — он выносится, потому что его границы уже автономны. В Molot: пять bounded contexts в одном бинаре, каждый со своей схемой и версионированными событиями. Вынести `billing` в отдельный сервис — замена одного адаптера, не переписывание.

```
internal/
    auction/   — своя схема, свои events/, свои миграции
    billing/   — то же
    settlement/
    participant/
    notification/
```

(Глава 1, глава 2.)

---

## Группа 11. Тактический DDD без стратегического

**Как выглядит.** Entity, Repository, Value Object — есть. Event Storming — нет. Границы нарезаны «по похожести сущностей»: `Invoice` и `Auction` в одном модуле, потому что «оба про деньги». Стейкхолдеры узнают границы по факту, когда уже дорого менять.

**Чем кончается.** Неправильная граница — это граница, которую придётся пересекать при каждой новой фиче. «Добавить второй шанс бидеру» затрагивает auction, billing, settlement и notification одновременно — не потому что задача сложная, а потому что границы проведены не там. Литмус-тест: если новая фича не помещается в один модуль — граница неправильная.

**Как правильно.** Стратегический DDD первым: доменный поток (аналог Event Storming), артефакт хранится в репо, границы — там, где меняется ответственность и язык. Потом тактика. В Molot: `settlement` существует отдельно от `auction` именно потому, что «расчёт после молотка» — отдельная ответственность с отдельным языком (settlement, compensation, second-chance) и отдельным темпом изменений.

(Глава 2.)

---

## Группа 12. BDUF и код «на будущее»

**Как выглядит.** «Нам потом понадобится event sourcing — заложим сейчас». Месячные refactoring-ветки без мержа в main. «Нужно было сделать правильно с самого начала» — и написан фреймворк вместо продукта. «Пока пишем, всё равно всё изменится» — и не пишется ничего, кроме абстракций.

**Чем кончается.** Бюджет изменений (время, внимание команды) потрачен на задачи, которые никогда не понадобились. Долгоживущие ветки означают мержевые конфликты и потерянный контекст. «Код на будущее» создаёт сложность сегодня в обмен на гипотетическое удобство завтра — трейдофф, который почти никогда не отрабатывает.

**Как правильно.** MVP-инкременты. Код «на будущее» отклоняется — вместо него дизайн, в котором будущее легко добавить. В Molot отдельного read store нет — читаем из той же БД. Когда понадобится — встанет заменой адаптера: query handler объявляет интерфейс `AvailableAuctionsReadModel`, а откуда данные — скрыто за этим интерфейсом.

```go
// internal/auction/app/query/active_catalog.go
// Query handler объявляет интерфейс у себя — реализация меняется без касания этого файла
type ActiveCatalogReadModel interface {
    ActiveAuctions(ctx context.Context, q ActiveCatalog) ([]AuctionSummary, error)
}
```

(Глава 15, глава 14.)

---

## Группа 13. Тестовые грехи

Восемь конкретных ошибок, каждая с последствием.

**Ice-cream cone (e2e как основа).** Большинство тестов — end-to-end, мало unit. Каждый прогон — 20 минут, каждый второй флакает на сети. Команда перестаёт запускать тесты локально. *Правильно:* пирамида — домен unit без инфраструктуры, e2e — пара critical paths.

**Тесты вне CI.** «Пока не настроили CI — запускаем руками». Через две недели никто не запускает. *Правильно:* merge-gate с первого дня.

**`sleep` и растущие ретраи как лечение флаков.** `time.Sleep(100 * time.Millisecond)` в тесте — это признание, что тест не знает, когда система готова. Следующий разработчик увидит флак и увеличит sleep до 500ms. *Правильно:* `assert.Eventually` или синхронизация каналами.

**Cleanup данных между тестами.** `defer cleanupDB()` означает, что тест зависит от порядка выполнения. В параллельных тестах cleanup убивает данные соседнего теста. *Правильно:* изоляция уникальными ID, никогда не чистим.

**Тестирование мока.** `assert.Equal(t, 1, mock.CallCount())` — это тест того, что мок был вызван. Не того, что система работает правильно. *Правильно:* тест проверяет наблюдаемый эффект, а не внутреннюю механику.

**Loop-var capture в `t.Parallel()`.** Классическая Go-ловушка: все итерации замыкания захватывают одну и ту же переменную. Тесты зелёные, но проверяют один и тот же case. *Правильно:* `c := testCases[i]` внутри цикла (или Go 1.22+).

**100%-coverage как цель.** Карго-культ: 100% coverage достигается тестами, которые исполняют каждую строку, но не проверяют инварианты. *Правильно:* 70–80% для Go — хорошо; критерий — «насколько легко сломать то, что охраняет тест».

**Тесты-зеркала реализации.** Тест проверяет: «метод вызвал именно эти методы в именно таком порядке». Рефакторинг реализации без изменения поведения — сотня упавших тестов. *Правильно:* тест проверяет поведение через публичный API, не внутреннюю механику.

В Molot: `t.Parallel()` везде, изоляция UUID, обязательный rollback-тест и race-тест (20 горутин, один победитель), `-race` в `make test`, один shared suite на Postgres и in-memory. (Глава 11.)

---

## Группа 14. Вера в то, что строгий контракт гарантирует качество данных

**Как выглядит.** «Используем gRPC/protobuf — значит данные всегда корректны». Валидация заканчивается на том, что поле `string` не nil. В системе ходят непустые бессмысленные значения: `amount = 0`, `currency = ""`, `bidder_id = "undefined"`.

**Чем кончается.** Строгий контракт гарантирует только то, что данные правильно сериализованы. Бизнес-инварианты он не проверяет. `amount = 0` пройдёт через gRPC без проблем и создаст ставку на ноль рублей — если домен не отклонит. Через полгода база полна «правильно оформленных» мусорных данных.

**Как правильно.** Три слоя валидации: контракт (proto constraints / OpenAPI validation), contract tests (потребитель проверяет, что продюсер держит обещания), доменные инварианты (валидирующий конструктор). Каждый слой своё. В Molot: конструктор `New(...)` отклоняет нулевые amount, пустые ID, некорректные currency — вне зависимости от того, как данные пришли.

```go
// internal/auction/domain/auction/auction.go
func New(..., startPrice, increment Money, ...) (*Auction, error) {
    if startPrice.IsZero() || startPrice.AmountMinor() <= 0 {
        return nil, ErrInvalidStartPrice
    }
    if startPrice.Currency() != rules.Currency() {
        return nil, ErrUnsupportedCurrency
    }
    // ...
}
```

(Глава 4, глава 6.)

---

## Группа 15. Самописная аутентификация

**Как выглядит.** Собственная реализация хэширования паролей (`md5(password + "salt")`), собственная генерация токенов, собственный протокол сессий. «Временный» хардкод пароля администратора для тестирования.

**Чем кончается.** OWASP Top 10 #2 — Cryptographic Failures. Самописная крипто неизбежно содержит уязвимости, которые найдёт не penetration-тест, а злоумышленник в production. «Временный» бэкдор остаётся навсегда. Не потому что разработчики некомпетентны — а потому что криптографические ошибки требуют экспертизы, которой нет у большинства команд.

**Как правильно.** Стандартный JWT с библиотекой, один middleware, всё что сложнее — провайдер (Auth0, Keycloak, AWS Cognito). В Molot: `golang-jwt/jwt` с HS256, `NewMiddleware` принимает режим из конфига и явно отклоняет неизвестные режимы с actionable-ошибкой.

```go
// internal/common/auth/auth.go
func NewMiddleware(mode, hs256Secret string) (func(http.Handler) http.Handler, error) {
    switch mode {
    case ModeLocalHS256:
        if hs256Secret == "" {
            return nil, errors.New("auth: AUTH_HS256_SECRET must not be empty when AUTH_MODE=local-hs256")
        }
        return hs256Middleware([]byte(hs256Secret)), nil
    case ModeJWKS:
        return nil, errors.New("auth: AUTH_MODE=jwks is not implemented yet: run with AUTH_MODE=local-hs256 ...")
    default:
        return nil, fmt.Errorf("auth: unknown AUTH_MODE %q", mode)
    }
}
```

(Глава 13.)

---

## Группа 16. Срезание углов как привычка и его зеркало — over-engineering

Это два полюса одной ошибки, и они одинаково разрушительны.

**Срезание углов как привычка.** Не осознанное прагматичное решение («у нас MVP, вернёмся через спринт»), а рефлекс. Отсутствие тестов, прямая мутация полей агрегата, `TODO: fix this properly`, транзакция через context — не потому что «сейчас некогда», а потому что «и так сойдёт». Разница в том, что осознанный shortcut возвращается и закрывается. Рефлекс — нет.

**Чем кончается.** Команда начинает бояться менять код. Каждая правка ломает что-то в соседней комнате. Тест-сьют не запускается, потому что он красный уже два месяца. Новый разработчик не может понять, что «правильно» в этом коде — потому что примеров правильного нет.

**Over-engineering — симметричная ошибка.** Полный DDD-стек на CRUD-модуль для хранения email-адресов. Event sourcing, потому что «звучит серьёзно». Командная шина внутри монолита. Шесть уровней абстракции для `SELECT * FROM users WHERE id = ?`. Бюджет изменений тратится на борьбу с собственной архитектурой, а не на бизнес-задачи.

**Как правильно.** Паттерн применяется пропорционально сложности того, что он защищает. Исключение документируется и пересматривается при росте логики. В Molot: `notification` — без domain/app слоёв: событие → шаблон → отправка. `participant` — слои есть, но без проекций. Исключение написано прямо в пакете.

```go
// internal/notification/service/service.go
// Package service is the composition root of the notification context:
// it wires the fake email sender and the Postgres stores into the event
// handlers... The context has no HTTP API and no facade — it is a pure
// event consumer.

// internal/participant/service/service.go
// The context publishes ParticipantRegisteredV1/ParticipantVerifiedV1
// and subscribes to nothing, so there is no RegisterEventHandlers;
// no other context calls participant synchronously, so there is no Facade.
```

(Глава 14.)

---

## Чек-лист самопроверки

Перед code review проверьте по этому списку. Каждый «да» — повод для разговора с командой.

**Модель**
- [ ] Есть публичные поля или сеттеры на доменном типе?
- [ ] Бизнес-`if` в хендлере или app-слое (не в домене)?
- [ ] Состояние кодируется строками или булевыми парами вместо value objects?
- [ ] Конструктор без валидации или с частичной валидацией?

**Слои**
- [ ] `db:` или `json:` теги на доменном типе?
- [ ] Один struct используется и для хранения, и для API?
- [ ] App-пакет импортирует адаптеры?
- [ ] HTTP-статус возвращается из use case или домена?

**Репозиторий и транзакции**
- [ ] Транзакция в `context.Value` или прокидывается через параметры?
- [ ] `updateFn` мутирует переданный указатель вместо возврата нового значения?
- [ ] Нет `SELECT ... FOR UPDATE` при read-modify-write?
- [ ] Репозиторий содержит бизнес-правила или валидацию?
- [ ] В интерфейсе repo есть per-use-case методы?

**Безопасность**
- [ ] Авторизация — ручные `if` в хендлерах?
- [ ] Системные потоки используют fake user с нужными правами?
- [ ] Identity берётся из `context.Value` внутри repo?
- [ ] Самописная крипто или хардкод секретов?

**Тесты**
- [ ] Большинство тестов — e2e (перевёрнутая пирамида)?
- [ ] `sleep` в тестах?
- [ ] Cleanup данных между тестами?
- [ ] Тест проверяет, что мок был вызван, а не наблюдаемый эффект?
- [ ] Loop-var без копирования в `t.Parallel()`?

**Архитектура**
- [ ] Новая фича открывает больше одного модуля?
- [ ] Микросервисы создаются до понимания доменных границ?
- [ ] Добавлен код «на будущее» без конкретной потребности?

---

## Сводная таблица: запах → глава книги

| Запах в коде | Антипаттерн | Глава |
|---|---|---|
| Struct с методом вместо пакетной функции | «Java в Golang» | Глава 4 |
| Публичные поля, сеттеры, magic strings | Anemic model | Глава 4 |
| Хендлер 300+ строк, `Update(bool, bool)` | Eight-thousanders | Глава 7 |
| `db:` теги на доменном типе | DB на домене | Глава 3, 5 |
| Один struct для хранения и API | Один struct на всё | Глава 3 |
| Общий тип для двух причин изменений | DRY на данных | Глава 3 |
| `context.WithValue(ctx, "tx", tx)` | Транзакции через context | Глава 5 |
| `ApproveReschedule(...)` на repo | Per-use-case repo | Глава 5 |
| `if user.Role != "admin"` в хендлере | Guard-if авторизация | Глава 13 |
| Бизнес-`if` в `Handle(...)` | Бизнес-if в app | Глава 4, 7 |
| Синхронный вызов соседнего сервиса везде | Микросервисы как лекарство | Глава 1, 2 |
| Границы по «похожести» сущностей | Тактический DDD без стратегического | Глава 2 |
| Долгоживущие ветки, код «на будущее» | BDUF/перфекционизм | Глава 15, 14 |
| `time.Sleep` в тестах | Тестовые грехи | Глава 11 |
| `assert(mock.CallCount() == 1)` | Тест мока | Глава 11 |
| Перевёрнутая пирамида (e2e как основа) | Ice-cream cone | Глава 11 |
| gRPC есть, валидации домена нет | Вера в контракт | Глава 4, 6 |
| Самописный JWT, хардкод секрета | Самописная аутентификация | Глава 13 |
| `TODO: refactor`, прямая мутация везде | Срезание углов как привычка | Глава 0, 14 |
| DDD-стек на тривиальном CRUD | Over-engineering | Глава 14 |
