# Глава 19. Практикум: фича сквозь все слои

## Зачем читать эту главу

Восемнадцать глав объясняли решения по отдельности: агрегаты, updateFn, outbox, проекции, таксономия тестов. Эта глава — проверка, что из деталей собирается машина. Мы пройдём **полный цикл добавления фичи** — от формулировки на языке домена до строчки в OpenAPI-спеке — и посчитаем, во сколько файлов и строк это обошлось. Затем — упражнения: на поиск гарантий в коде, на саботаж, на проектирование и на аргументацию в ревью.

Фича выбрана не игрушечная: она трогает каждый слой, взаимодействует с анти-снайп-механикой и закрывающим воркером, требует нового интеграционного события и правки проекций. При этом она **не реализована в репозитории** — все наброски кода ниже помечены «эскиз». Идеальный режим чтения: репозиторий открыт рядом, и каждый шаг вы примеряете к реальным файлам.

---

# Часть 1. Walkthrough: «Продавец продлевает аукцион»

## Шаг 0. Формулировка на языке домена

Просьба бизнеса звучала бы так: «Продавец видит, что торги идут вяло, и хочет дать лоту ещё времени. Разрешим продлить идущий аукцион — но один раз, и только пока не началась финальная фаза, иначе продавцы будут дёргать дедлайн под носом у снайперов».

Переводим в Ubiquitous Language (глава 2, правило 53 [BOOK_AUDIT](../BOOK_AUDIT.md)):

- **Команда**: «Продлить аукцион» (`ExtendAuctionBySeller`). Не «обновить ends_at» — это технический жаргон, который потерял бы половину правил.
- **Актор**: продавец. Только владелец лота, как у `Cancel`.
- **Политика**: однократно; только пока аукцион идёт; **только до входа в снайп-окно** — внутри него дедлайном владеет анти-снайп-механика (§2.1 [ARCHITECTURE](../ARCHITECTURE.md)).
- **Доменное событие** (факт в прошедшем времени): «Аукцион продлён продавцом» (`AuctionExtendedBySeller`).

Первая правка — вообще не код. Правило 51 требует держать артефакт discovery аудируемым: в [docs/event-storming.md](../event-storming.md) в хронологический поток между «ставка сделана» и «молоток» добавляется событие «аукцион продлён продавцом», команда, актор и политика снайп-окна; в таблицу маппинга «событие → Go-тип» — будущая строка `AuctionExtendedBySeller`. Литмус-тест границы (правило 52): фича целиком живёт в контексте `auction` — ни billing, ни settlement о ней знать не должны. Если бы оказалось иначе — чинили бы границу, а не писали код.

## Шаг 1. Доменная модель: guard, sentinel-ошибки, событие

**Куда**: `internal/auction/domain/auction/auction.go`, `errors.go`, `events.go`.

**Почему сюда первым делом**: правило ставки живёт в `PlaceBid`, правило отмены — в `Cancel`; правило продления обязано жить рядом — behavior-метод, в котором guard инварианта и переход состояния атомарны (глава 5, правила 8–9). Любой `if` про «можно ли продлить», оставленный в хендлере или порту, — формальный дефект (правило 14).

Агрегату нужно новое состояние — «продавец уже продлевал». Переиспользовать `extensionsUsed` нельзя: это бюджет *анти-снайп*-продлений, снапшот платформенной политики; смешав счётчики, мы бы молча украли у лота защиту от снайпинга. Значит, отдельное поле `sellerExtended bool` — по образцу `winnerReassigned`.

```go
// эскиз, не из репозитория — internal/auction/domain/auction/auction.go

// maxSellerExtension — потолок ручного продления (политика платформы).
const maxSellerExtension = 24 * time.Hour

// ExtendBySeller однократно продлевает идущий аукцион по воле продавца.
// Доступно только до входа в снайп-окно: внутри него дедлайном владеет
// анти-снайп-механика, и ручное продление стало бы способом дёргать
// дедлайн под уже летящими ставками.
func (a *Auction) ExtendBySeller(by SellerID, d time.Duration, now time.Time) error {
    if err := CanSellerManageAuction(by, *a); err != nil {
        return err
    }
    switch a.status {
    case StatusCancelled:
        return ErrAuctionCancelled
    case StatusClosed:
        return ErrAlreadyClosed
    case StatusListed:
        // proceed
    default:
        panic("auction: unknown status " + a.status.String())
    }
    if d <= 0 || d > maxSellerExtension {
        return ErrInvalidExtension
    }
    if !a.window.IsOpenAt(now) {
        return ErrAuctionNotOpen
    }
    if a.antiSnipe.TriggersAt(now, a.window.EndsAt()) {
        return ErrTooLateToExtend
    }
    if a.sellerExtended {
        return ErrAlreadyExtendedBySeller
    }

    a.window = a.window.ExtendedBy(d)
    a.sellerExtended = true
    a.record(AuctionExtendedBySeller{NewEndsAt: a.window.EndsAt(), OccurredAt: now})
    return nil
}
```

Разберём решения:

- **Порядок guard-ов — часть контракта** (глава 5). Авторизация первой, как в `Cancel`: чужак получает `not-seller`, не узнавая попутно состояние лота. Затем статус (switch с паникующим `default` — правило 12), затем валидация входа, затем временные правила.
- **`TriggersAt` переиспользован** из `antisnipe.go:29` — он отвечает ровно на нужный вопрос «мы уже в снайп-окне?». Заметьте бонус: `TriggersAt` истинен и *после* `endsAt`, но кейс «окно уже истекло, воркер ещё не закрыл» перехватывается раньше через `IsOpenAt` — тем же guard-ом, что у `PlaceBid`, и с той же ошибкой `ErrAuctionNotOpen`. Продление в эту щель «воскресило» бы де-факто завершённые торги.
- **`ExtendedBy` переиспользован** из `schedule.go:46` — он уже сохраняет `originalEndsAt`.
- Sentinel-ошибки — экспортируемые package-level значения в `errors.go` (правило 10):

```go
// эскиз, не из репозитория — internal/auction/domain/auction/errors.go
ErrAlreadyExtendedBySeller = errors.New("auction has already been extended by the seller")
ErrTooLateToExtend         = errors.New("too late to extend: the snipe window has begun")
ErrInvalidExtension        = errors.New("extension must be positive and within the platform cap")
```

- Доменное событие в `events.go` + маркер `isDomainEvent()`:

```go
// эскиз, не из репозитория — internal/auction/domain/auction/events.go

// AuctionExtendedBySeller — recorded by ExtendBySeller. В отличие от
// AuctionExtended (анти-снайп) имеет интеграционный маппинг: никакое
// другое событие не несёт наружу новый дедлайн.
type AuctionExtendedBySeller struct {
    NewEndsAt  time.Time
    OccurredAt time.Time
}

func (AuctionExtendedBySeller) isDomainEvent() {}
```

Плюс механика: поле `sellerExtended` в struct, предикат `SellerExtended() bool` для маппинга и +1 параметр в `UnmarshalFromDatabase` (она — единственный вход для адаптеров, правило 8; обойти её и собрать агрегат литералом адаптер физически не может).

**Вопросы на подумать** (ответьте до шага 2 — они станут тест-кейсами):

1. *Взаимодействие с анти-снайпом.* Сбрасывает ли ручное продление бюджет `extensionsUsed`? Нет: бюджеты независимы — анти-снайп защищает бидеров, ручное продление — воля продавца. После `ExtendBySeller` снайп-окно «переезжает» к новому дедлайну, и анти-снайп продолжает работать как ни в чём не бывало. Это надо зафиксировать тестом, иначе следующий рефакторинг молча свяжет счётчики.
2. *`originalEndsAt`.* `BiddingWindow.ExtendedBy` его не трогает. Для анти-снайпа это правильно: поле отвечает «когда торги кончились бы без вмешательств». Но ручное продление — тоже вмешательство, и теперь поле фактически означает «дедлайн на момент листинга». Имя начинает слегка врать. Варианты: оставить и переименовать в `listedEndsAt` при следующей миграции — или двигать `originalEndsAt` вместе с ручным продлением, и тогда история анти-снайпа считается от нового «планового» дедлайна. Molot-стиль — первый вариант: меньше специальных случаев в `UnmarshalBiddingWindow` (там guard `endsAt >= originalEndsAt`, проверьте, что он переживает оба варианта).
3. *Гонка «продление против снайп-ставки».* Обе мутации идут через `Update` → `SELECT ... FOR UPDATE` (глава 7) и сериализуются. Ставка первой → окно продлилось анти-снайпом → продавец получает `ErrTooLateToExtend`. Продление первое → ставка уже не в снайп-окне → не triggers. Оба порядка консистентны; доказывать новым race-тестом не обязательно — механизм тот же, что в «close vs snipe bid» (`repository_suite_test.go:316`).
4. *Где жить потолку `maxSellerExtension`?* Константа — самое простое; консистентнее со снапшот-философией §2.1 — поле в `ListingRules` (как `antiSnipe`), снапшотится при листинге. Тогда меняем политику платформы — идущие аукционы живут по старой. Для MVP-инкремента (правило 54) константы достаточно; ListingRules — задокументированный следующий шаг.

## Шаг 2. Доменные тесты: table-driven

**Куда**: `internal/auction/domain/auction/extend_by_seller_test.go` — рядом с `place_bid_test.go`, в том же black-box пакете `auction_test` (глава 13, правило 40). Фикстуры — только через доменный API: `listedAuction(t)`, `placeBid(t, ...)` из `fixtures_test.go` уже умеют всё нужное.

Минимальная таблица кейсов (формат — как в `TestAuction_PlaceBid`):

| # | Кейс | Ожидание |
|---|------|----------|
| 1 | продавец продлевает идущий аукцион | nil; `EndsAt` сдвинут ровно на `d`; записано `AuctionExtendedBySeller` |
| 2 | не-продавец продлевает | `ForbiddenAuctionManagementError` (через `errors.As`) |
| 3 | второе продление | `ErrAlreadyExtendedBySeller` |
| 4 | продление внутри снайп-окна | `ErrTooLateToExtend` |
| 5 | граница: `now == endsAt − snipeWindow` | `ErrTooLateToExtend` (`TriggersAt` инклюзивен — `!now.Before(...)`) |
| 6 | за миг до границы: `now == endsAt − snipeWindow − 1ns` | nil |
| 7 | после `endsAt`, но воркер ещё не закрыл | `ErrAuctionNotOpen` |
| 8 | до открытия окна (`now < startsAt`) | `ErrAuctionNotOpen` |
| 9 | закрытый аукцион | `ErrAlreadyClosed` |
| 10 | отменённый аукцион | `ErrAuctionCancelled` |
| 11 | нулевая/отрицательная длительность; длительность > потолка | `ErrInvalidExtension` |
| 12 | продление не трогает `extensionsUsed`; снайп-ставка у нового дедлайна всё ещё продлевает окно | анти-снайп жив |
| 13 | после анти-снайп-продления ручное всё ещё доступно (вне нового снайп-окна) | nil — бюджеты независимы |
| 14 | `originalEndsAt` не изменился | равен дедлайну листинга |

Кейсы 5–6 — классическая проверка границы инклюзивности: они стоят три строки, а ловят самую дорогую ошибку (`>=` против `>`). Кейсы 12–14 — материализация «вопросов на подумать» из шага 1: тест фиксирует решение, а не только код.

Ни одного мока, ни одной горутины, ни одного `sleep` — время передаётся параметром `now` (глава 6). Вся таблица прогоняется за миллисекунды.

## Шаг 3. Команда и хендлер

**Куда**: `internal/auction/app/command/extend_auction_by_seller.go` — один use case = один файл (правило 26). Плюс одна строка в `internal/auction/app/app.go` (поле в `Commands`) и три строки сборки в `internal/auction/service/service.go` (обёртка в декораторы — логирование, RED-метрики, трейсинг достаются бесплатно, правило 29; глава 14).

```go
// эскиз, не из репозитория — internal/auction/app/command/extend_auction_by_seller.go

// ExtendAuctionBySeller продлевает идущий аукцион по воле продавца —
// однократно и только до входа в снайп-окно.
type ExtendAuctionBySeller struct {
    AuctionID auction.AuctionID
    Seller    auction.SellerID
    Extension time.Duration
}

type ExtendAuctionBySellerHandler struct {
    repo  auction.Repository
    clock clock
}

func NewExtendAuctionBySellerHandler(repo auction.Repository, clock clock) ExtendAuctionBySellerHandler {
    if repo == nil {
        panic("NewExtendAuctionBySellerHandler: nil repo")
    }
    if clock == nil {
        panic("NewExtendAuctionBySellerHandler: nil clock")
    }
    return ExtendAuctionBySellerHandler{repo: repo, clock: clock}
}

func (h ExtendAuctionBySellerHandler) Handle(ctx context.Context, cmd ExtendAuctionBySeller) error {
    err := h.repo.Update(ctx, cmd.AuctionID, auction.ActorFromSeller(cmd.Seller),
        func(ctx context.Context, a *auction.Auction) (*auction.Auction, error) {
            if err := a.ExtendBySeller(cmd.Seller, cmd.Extension, h.clock.Now()); err != nil {
                return nil, err
            }
            return a, nil
        })
    if err != nil {
        return mapExtendError(err)
    }
    return nil
}

func mapExtendError(err error) error {
    if mapped, ok := mapNotFound(err); ok {
        return mapped
    }
    var forbidden auction.ForbiddenAuctionManagementError
    switch {
    case errors.As(err, &forbidden):
        return errs.NewForbiddenError("not-seller").WithCause(err)
    case errors.Is(err, auction.ErrAlreadyExtendedBySeller):
        return errs.NewConflictError("already-extended").WithCause(err)
    case errors.Is(err, auction.ErrTooLateToExtend):
        return errs.NewConflictError("too-late-to-extend").WithCause(err)
    case errors.Is(err, auction.ErrAuctionNotOpen):
        return errs.NewConflictError("auction-not-open").WithCause(err)
    case errors.Is(err, auction.ErrAlreadyClosed):
        return errs.NewConflictError("already-closed").WithCause(err)
    case errors.Is(err, auction.ErrAuctionCancelled):
        return errs.NewConflictError("auction-cancelled").WithCause(err)
    case errors.Is(err, auction.ErrInvalidExtension):
        return errs.NewIncorrectInputError("invalid-extension").WithCause(err)
    default:
        return err
    }
}
```

Здесь нет ни одного бизнес-`if` — хендлер оркеструет (правило 14): актор — `ActorFromSeller` (правило 21; глава 15), время — из `clock` (consumer-side интерфейс в `deps.go`, глава 6), доменные sentinel-ы заворачиваются в транспортно-агностичные slug-ошибки (правило 30; глава 8). Заметьте: в отличие от saga-команд (`ConfirmSettlement` и компания, глава 10) здесь повтор **не** маппится в nil — для пользовательской команды `already-extended` это честный конфликт, который клиент должен увидеть, а не тихий no-op.

**App-тест со spy** — дописывается в `internal/auction/app/command/handlers_test.go`, инфраструктура (`newSpyRepo`, `fixedClock`, `listedAuction`) уже есть:

```go
// эскиз, не из репозитория
t.Run("extends with the seller as actor", func(t *testing.T) {
    t.Parallel()
    repo := newSpyRepo()
    a := listedAuction(t)
    endsBefore := a.EndsAt()
    repo.seed(a)
    h := command.NewExtendAuctionBySellerHandler(repo, fixedClock{now0.Add(time.Hour)})

    err := h.Handle(context.Background(), command.ExtendAuctionBySeller{
        AuctionID: a.ID(), Seller: a.Seller(), Extension: 2 * time.Hour,
    })
    if err != nil {
        t.Fatal(err)
    }
    call := repo.updates[0]
    if call.system || call.actor != auction.ActorFromSeller(a.Seller()) {
        t.Fatalf("update call = %+v, want user update by the seller", call)
    }
    got, _ := repo.Get(context.Background(), a.ID())
    if !got.EndsAt().Equal(endsBefore.Add(2 * time.Hour)) {
        t.Fatal("extension must be persisted through updateFn")
    }
})
```

Тестируем **только оркестрацию** (правило 41): что вызван `Update` (не `UpdateAsSystem`!), что актор — продавец, что доменные ошибки превращаются в нужные slug-и. Кейсы «когда продлевать нельзя» здесь не повторяются — они уже исчерпаны таблицей шага 2; их дубль в app-тестах означал бы, что логика утекла из домена.

## Шаг 4. Интеграционное событие: нужен ли V1 наружу?

**Куда**: `internal/auction/events/events.go` и `internal/auction/adapters/events_mapper.go`.

Сначала — нужно ли вообще публиковать событие за пределы агрегата? Проверка простая: есть ли у изменения потребители вне write-модели. Есть: каталог (`auction.catalog_items`) и дашборд продавца показывают `ends_at` — им нужно узнать о новом дедлайне (§5; глава 8 — проекции наполняются только событиями, читать чужую write-таблицу из обработчика проекции нельзя даже внутри контекста, это сломало бы путь будущего выноса read store).

Соблазн: «дедлайн уже умеет возить `BidPlacedV1` — у него есть `NewEndsAt`, переиспользуем!» Нельзя, и полезно понять все три причины:

1. **Семантика.** `BidPlacedV1` означает «ставка сделана». Ставки не было. Событие-ложь отравляет каждого будущего подписчика: счётчики ставок, аналитику, нотификации «вас перебили».
2. **Механика.** Проекция `ApplyBid` (`catalog_pg_projection.go:58`) защищена монотонным guard-ом `ci.bid_count < $3`. Продление не увеличивает `bid_count` — «фейковый» `BidPlacedV1` с тем же счётчиком был бы молча отброшен как stale-дубликат. Guard, спасающий от redelivery, корректно убил бы нашу подделку.
3. **Контракт.** Схемы V1 append-only (правило 34; глава 9): дотачивать опубликованное событие под новый смысл — то самое редактирование, которое запрещено. Отдельное событие честнее и дешевле.

```go
// эскиз, не из репозитория — internal/auction/events/events.go
type AuctionExtendedBySellerV1 struct {
    EventID    string    `json:"event_id"`
    AuctionID  string    `json:"auction_id"`
    SellerID   string    `json:"seller_id"`
    NewEndsAt  time.Time `json:"new_ends_at"`
    OccurredAt time.Time `json:"occurred_at"`
}
```

Плоская структура, примитивы + `time.Time`, версия в имени. Никакого `sellerExtended`-флага наружу: подписчикам важен факт и новый дедлайн, не внутренняя бухгалтерия агрегата.

Теперь самое приятное: маппер `mapDomainEvent` (`events_mapper.go:21`) **заставит** вас принять решение. `DomainEvent` — закрытое множество, `default`-ветка паникует: новое доменное событие без явного маппинг-решения — programmer error, который упадёт на первом же интеграционном тесте, а не потеряется молча. Добавляем case:

```go
// эскиз, не из репозитория — internal/auction/adapters/events_mapper.go
case auction.AuctionExtendedBySeller:
    return auctionevents.AuctionExtendedBySellerV1{
        EventID:    uuid.NewString(),
        AuctionID:  a.ID().String(),
        SellerID:   a.Seller().String(),
        NewEndsAt:  e.NewEndsAt,
        OccurredAt: e.OccurredAt,
    }
```

Сравните с соседним `case auction.AuctionExtended: return nil` — анти-снайп-продление осталось domain-only, потому что его дедлайн возит `BidPlacedV1`. Два внешне похожих события — два разных маппинг-решения, и оба явные.

## Шаг 5. Repository: почему (почти) ничего не меняется

Самый показательный шаг — тем, как мало в нём работы.

**Интерфейс `auction.Repository` (`domain/auction/repository.go`) не меняется вообще.** Никакого `ExtendAuction(ctx, id, ...)` на репозитории — это был бы per-use-case метод, антипаттерн из главы 7 (правило 15): репозиторий начал бы впитывать семантику приложения, и каждая следующая фича правила бы интерфейс, обе реализации и общий тест-сьют. Семантика продления приехала в универсальный `Update` внутри замыкания ещё на шаге 3 — updateFn-паттерн **поглощает новые мутации by design**. Вся машинерия — `SELECT ... FOR UPDATE`, optimistic version, rollback на ошибке замыкания, append-only история ставок, публикация в outbox в той же транзакции (`auction_pg_repository.go:176`) — обслуживает новую команду без единой правки. Транзакционность и сериализация гонок (вопрос 3 из шага 1) достались фиче бесплатно.

Честная оговорка: у агрегата появилось новое **персистентное поле**, и снапшот обязан его пережить. Это три механические правки в одном адаптере: миграция `internal/auction/adapters/migrations/00002_seller_extension.sql` (`ALTER TABLE auction.auctions ADD COLUMN seller_extended boolean NOT NULL DEFAULT false`), колонка в transport-структуре `pgAuction` + в `auctionColumns` + в UPDATE-стейтменте, и +1 аргумент в вызове `UnmarshalFromDatabase`. Ни строчки новой *логики* — только отражение нового состояния. In-memory реализация (`auction_inmem_repository.go`) не меняется совсем: она хранит доменные значения, а не свою схему.

И последний бесплатный подарок: закрывающий воркер (глава 12) сканирует `ends_at` write-таблицы (`DueForClosing`, `auction_pg_repository.go:254`) — продлённый аукцион автоматически выпадает из кандидатов. А если воркер успел захватить кандидата до продления — `Close` вернёт `ErrBiddingStillOpen`, который хендлер закрытия уже маппит в nil (см. тест «still open maps to nil (extension race)» в `handlers_test.go:179`). Гонка «воркер против продления» была решена до того, как фичу придумали, — потому что guard живёт в домене, а не в воркере.

## Шаг 6. HTTP: спека → регенерация → тонкий хендлер

**Куда**: `api/openapi/auction.yaml`, затем `make generate`, затем `internal/auction/ports/http.go`.

Contract-first (глава 16): сначала спека, потом код. Ресурс именуем по образцу `/auctions/{auctionID}/cancellation` — команда как существительное-ресурс, не `PATCH /auctions/{id}` с телом-простынёй:

```yaml
# эскиз, не из репозитория — api/openapi/auction.yaml
  /auctions/{auctionID}/extension:
    post:
      operationId: ExtendAuction
      summary: Extend a running auction once (seller only, before the snipe window)
      parameters:
        - $ref: "#/components/parameters/AuctionID"
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: "#/components/schemas/ExtendAuctionRequest"
      responses:
        "204":
          description: Auction extended
        "400":
          $ref: "#/components/responses/Error"
        "401":
          $ref: "#/components/responses/Error"
        "403":
          description: not-seller
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Error"
        "404":
          $ref: "#/components/responses/Error"
        "409":
          description: already-extended | too-late-to-extend | auction-not-open | already-closed | auction-cancelled
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Error"
# components/schemas:
    ExtendAuctionRequest:
      type: object
      required: [extensionMinutes]
      properties:
        extensionMinutes:
          type: integer
          minimum: 1
```

Слаги в `description` 403/409 — те самые, что производит `mapExtendError`: контракт ошибок читается из спеки, клиент ветвится по slug-у, не по тексту. `make generate` перегенерирует `internal/auction/ports/openapi.gen.go` (руками не правится — правило 55), и компиляция немедленно падает: `HTTPServer` больше не реализует `StrictServerInterface`. Несоответствие контракту — ошибка компиляции, а не сюрприз в проде. Дописываем тонкий хендлер:

```go
// эскиз, не из репозитория — internal/auction/ports/http.go
func (s HTTPServer) ExtendAuction(ctx context.Context, request ExtendAuctionRequestObject) (ExtendAuctionResponseObject, error) {
    user, err := auth.UserFromCtx(ctx)
    if err != nil {
        return nil, err
    }
    seller, err := auction.NewSellerID(user.ID)
    if err != nil {
        return nil, errs.NewIncorrectInputError("invalid-id").WithCause(err)
    }
    auctionID, err := auction.NewAuctionID(request.AuctionID)
    if err != nil {
        return nil, errs.NewIncorrectInputError("invalid-id").WithCause(err)
    }

    if err := s.app.Commands.ExtendAuctionBySeller.Handle(ctx, command.ExtendAuctionBySeller{
        AuctionID: auctionID,
        Seller:    seller,
        Extension: time.Duration(request.Body.ExtensionMinutes) * time.Minute,
    }); err != nil {
        return nil, err
    }
    return ExtendAuction204Response{}, nil
}
```

Распаковать → собрать команду из доменных типов → `Handle` → 204. Идентичность — из JWT-контекста (`auth.UserFromCtx`), никогда из тела запроса: «кто продлевает» решает токен, не клиентский JSON (глава 15). Маппинг slug → HTTP-статус делает общий `httperr.RespondWithSlugError`, подключённый один раз в `service.RegisterHTTP` — в хендлере про статусы ни слова (правило 30). Кто проверяет «не слишком ли поздно»? Никто здесь: домен уже проверил. Кто проверяет `extensionMinutes >= 1`? Спека (схемой) и домен (`ErrInvalidExtension`) — порт не дублирует ни того, ни другого.

## Шаг 7. Проекция: новый дедлайн в каталоге

**Куда**: `internal/auction/ports/events.go` (метод в consumer-side интерфейсе `CatalogProjection` + регистрация хендлера) и `internal/auction/adapters/catalog_pg_projection.go` (реализация). Дашборду продавца дедлайн тоже виден — симметричный метод в `DashboardProjection`.

Доставка at-least-once, значит хендлер обязан быть идемпотентным (правило 37; глава 9). У `ApplyBid` монотонный guard по `bid_count`; продление счётчик не двигает — наш guard монотонен **по времени**:

```go
// эскиз, не из репозитория — internal/auction/adapters/catalog_pg_projection.go

// ApplyExtension сдвигает дедлайн карточки вперёд. Guard ends_at < $2
// делает redelivery и out-of-order доставку no-op-ом: дедлайн в
// каталоге движется только вперёд.
func (p *CatalogPostgresProjection) ApplyExtension(ctx context.Context, auctionID uuid.UUID, newEndsAt time.Time) error {
    res, err := p.db.ExecContext(ctx, `
        UPDATE auction.catalog_items
        SET ends_at = $2, updated_at = now()
        WHERE auction_id = $1 AND ends_at < $2`,
        auctionID, newEndsAt)
    if err != nil {
        return fmt.Errorf("unable to apply extension to catalog: %w", err)
    }
    affected, err := res.RowsAffected()
    if err != nil {
        return fmt.Errorf("unable to read rows affected: %w", err)
    }
    if affected > 0 {
        return nil
    }
    // Промах: stale-дубликат (ends_at уже >= нового) — ack; строка
    // удалена, потому что аукцион больше не listed — ack; строки ещё
    // нет (Listed не спроецирован) — errProjectionLag, retry.
    return p.classifyExtensionMiss(ctx, auctionID, newEndsAt)
}
```

Третья ветка промаха — не паранойя. У хендлеров `OnAuctionListed` и `OnAuctionExtendedBySeller` независимые consumer-group offset-ы: продление может приехать **раньше**, чем спроецирован сам листинг. Молча ack-нуть его — значит оставить карточке устаревший дедлайн из будущего `UpsertListed` до следующей ставки. Поэтому — тот же приём, что в `classifyMiss` (`catalog_pg_projection.go:88`): отличить «правильно отсутствует» от «ещё не доехал» и во втором случае вернуть `errProjectionLag`, чтобы Watermill ретраил. Регистрация — ещё один типизированный хендлер в `RegisterEventHandlers` (`ports/events.go`), консьюмер-группа `auction.OnAuctionExtendedBySeller`, внутри — `catalog.ApplyExtension` и `dashboard.ApplyExtension`.

## Шаг 8. Тесты: что добавить, что — нет

По таксономии главы 13 (правило 39), уровень за уровнем:

**Добавляем.**

- **Unit (domain)**: таблица шага 2 — `extend_by_seller_test.go`. Ядро защиты, ~14 кейсов, миллисекунды.
- **Unit (app)**: 3 сабтеста в `handlers_test.go` — актор/clock/persist (шаг 3), маппинг `too-late-to-extend`, `not-found`.
- **Integration**: (а) фикстура seller-extended агрегата в shared suite `repository_suite_test.go` — round-trip докажет, что колонка `seller_extended` переживает persist в **обеих** реализациях (правило 42); (б) идемпотентность `ApplyExtension` в `TestPostgresProjectionsAndReadModels` (`pg_integration_test.go:210`) — дважды одно продление → один сдвиг; stale `ends_at` → no-op; (в) outbox-тест увидит `AuctionExtendedBySellerV1` в топике после `Update` — расширение `TestPostgresOutbox`.
- **Component**: один шаг в существующем happy-path флоу `tests/component/auction_lifecycle_test.go` — list → extend → bid → close: проверяет wiring (роут → хендлер → команда → событие → проекция), не логику. Отдельный сценарий не нужен — corner cases исчерпаны ниже (правило 44).

**Не добавляем — и почему.**

- *Unit-тест HTTP-хендлера `ExtendAuction`*: это pass-through glue без ветвлений — его тестирование запрещает правило 41; ошибётся он — упадёт component-тест.
- *Тест маппера `mapDomainEvent` на новый case*: зеркалил бы реализацию построчно (правило 40); забытый case и так невозможен — `default` паникует, и это поймает первый же integration-прогон.
- *E2E*: продление — не critical path (ставка → молоток → счёт → оплата). E2E проверяют wiring прод-бинарей, не фичи (правило 45); раздувать их — путь к ice-cream cone.
- *Race-тест «extend vs bid»*: сериализацию конкурирующих `Update` уже доказывает suite (`race: 20 concurrent equal bids`, `race: close vs snipe bid`) — новый тест проверял бы тот же `FOR UPDATE` третий раз. Если сомневаетесь — добавьте, но осознайте, что покупаете.

## Шаг 9. Чек-лист самопроверки и итоговая смета

Прогон по [BOOK_AUDIT §10](../BOOK_AUDIT.md) — правила, которые фича реально задевает:

- **8, 9** — поле `sellerExtended` приватное; guard и переход атомарны в `ExtendBySeller`; сеттеров нет.
- **10** — три новых sentinel-а экспортированы; никто выше домена не перепроверяет снайп-окно.
- **14** — в хендлере и порту ноль бизнес-`if`.
- **15, 16** — интерфейс repo не тронут; мутация через `Update` + updateFn.
- **21, 22** — актор — явный типизированный параметр; авторизация — `CanSellerManageAuction` в домене, исполняется внутри транзакции.
- **26, 27** — команда named бизнес-языком, несёт доменные типы, не возвращает данных.
- **28, 30** — порт зовёт только хендлер; ошибки — slug-и, статусы — в одном общем хелпере.
- **34, 35** — `AuctionExtendedBySellerV1` плоский, версионированный, append-only; публикация — в транзакции агрегата через outbox.
- **37** — `ApplyExtension` идемпотентен, и это покрыто тестом.
- **40, 41** — domain-тесты black-box table-driven; app-тесты — только оркестрация на spy.
- **52, 53** — фича целиком в одном модуле; словарь — доменный.
- **55** — `.gen.go` перегенерирован Makefile-таргетом, руками не правился.

**Смета.** Затронуто ~15 файлов, все — внутри `internal/auction` плюс спека: домен 4 (метод+поле, ошибки, событие, тест-файл), app 3 (команда, каталог `app.go`, тесты), events 1, adapters 5 (маппер, миграция, pg-репозиторий — маппинг, две проекции), ports 3 (интерфейс+регистрация, http, перегенерированный `openapi.gen.go`), service 1, спека 1, event-storming 1. Порядка 120 строк продакшен-кода и 250–300 строк тестов и спеки. Ни одной правки в billing, settlement, participant, notification, common; ни одной — в воркере, саге, фасаде, транзакционной механике. Слои удержали изменение **локальным**: каждое решение фичи легло в заранее отведённое место, и самый сложный вопрос («что делать с гонками?») оказался решён архитектурой до постановки задачи. Это и есть тот возврат на инвестиции, ради которого писались главы 4–9.

---

# Часть 2. Упражнения

Уровни: ● — разминка (15–30 мин), ●● — вечер с репозиторием, ●●● — мини-проект или эссе на ревью. Подсказки спрятаны — сначала думайте.

### 1. Резервная цена не утекает ●
*Тип: найди в коде. Главы 8, 15.*

Резервная цена — секрет продавца. Найдите **все** механизмы, гарантирующие, что `reserve` не покидает write-модель: события, API-схему, read-модели. Составьте список «мест, где она могла бы утечь, но не утекает».

<details><summary>Подсказка</summary>

Четыре точки: `events/events.go` — `AuctionClosedV1` вместо резерва несёт готовый вердикт `RunnerUpQualifies` (комментарий прямо об этом); `adapters/events_mapper.go` — маппер не читает `a.Reserve()` ни в одном case; `api/openapi/auction.yaml` — в `AuctionCard` только `hasReserve: boolean`, а `reservePriceMinor` существует лишь в write-схеме `ListAuctionRequest` («never exposed in reads»); `adapters/auction_pg_read_models.go` — SELECT-ы карточки и каталога не включают колонку резерва. Бонусный вопрос: почему вердикт вычисляется в домене (`Close`, `auction.go:249`), а не подписчиком?
</details>

### 2. PlaceBid после Close невозможен ●
*Тип: найди в коде / докажи. Главы 5, 7.*

Докажите по коду, что ставка не может быть принята после удара молотка — даже при конкурентных запросах. Из каких **двух** независимых механизмов состоит гарантия?

<details><summary>Подсказка</summary>

(1) Домен: первый guard каскада `PlaceBid` — `a.status != StatusListed → ErrAuctionNotOpen` (`auction.go:157`), а `Close` атомарно ставит `StatusClosed`. (2) Конкуренция: обе мутации идут через `update()` с `SELECT ... FOR UPDATE` (`auction_pg_repository.go:183`) — транзакции сериализуются на строке, вторая видит уже закрытый агрегат. Живое доказательство — race-тест «close vs snipe bid — one consistent outcome» (`repository_suite_test.go:316`).
</details>

### 3. Ревью: сеттер «для тестов» ●
*Тип: обоснуй ревью. Главы 5, 13.*

PR добавляет `func (a *Auction) SetStatusForTest(s Status)`, «чтобы фикстуры не строить через пять вызовов». Напишите отказ: два архитектурных аргумента и конструктивная альтернатива.

<details><summary>Подсказка</summary>

Правила 8–9: экспортированный сеттер ломает always-valid инвариант для **всех** вызывающих, не только тестов — компилятор не различает намерения. Правило 40: фикстуры — только через доменный API; «пять вызовов» прячутся в хелпер (`closedSoldAuction(t, repo)` в `command/command_test.go` ровно так и сделан). Альтернатива: helper-фикстура в `fixtures_test.go`, прогоняющая агрегат через реальные `New → PlaceBid → Close`. Бонус: чем black-box пакет `auction_test` делает сеттер бесполезным даже технически?
</details>

### 4. Спроектируй: уведомление «вас перебили» ●
*Тип: спроектируй. Главы 2, 9.*

Бидеры хотят знать, что их ставку перебили. В каком контексте живёт фича? Какие события нужны? Сколько строк правок в контексте auction?

<details><summary>Подсказка</summary>

Ноль правок в auction: `BidPlacedV1` уже несёт `OutbidBidderID` (`events/events.go:37`) — контракт спроектирован с запасом. Фича целиком в notification: новый типизированный хендлер на `BidPlacedV1` (пустой `OutbidBidderID` — первая ставка, скип), идемпотентность — по `EventID`. Проверьте себя правилом 52: фича в одном модуле? Да — потому что граница была проведена правильно, а событие — спроектировано как контракт, не как DTO внутренностей.
</details>

### 5. Докажи отсутствие dual-write ●●
*Тип: найди в коде. Глава 9.*

Покажите по коду, что между «агрегат сохранён» и «событие опубликовано» не может упасть процесс так, чтобы одно случилось без другого. Найдите все три составляющие гарантии.

<details><summary>Подсказка</summary>

(1) `auction_pg_repository.go`: `publishMapped` вызывается на том же `tx`, что и UPDATE агрегата, внутри одного `postgres.RunInTx`. (2) Publisher — `watermill-sql` поверх той же БД: outbox-таблица коммитится атомарно с бизнес-данными. (3) `service/service.go:initializeOutboxTopic` — схема топика создаётся **до** первой транзакции, потому что `CREATE TABLE` внутри неё дал бы implicit commit (комментарий на месте). Контрольный вопрос: что произойдёт, если убрать (3) и топик создастся лениво?
</details>

### 6. Sabotage: транзакция, которая всегда коммитит ●●
*Тип: sabotage. Главы 7, 13.*

Сломайте `postgres.RunInTx` (`internal/common/postgres`): пусть он коммитит даже при ошибке из замыкания. Какой тест, какого уровня и с какой формулировкой упадёт первым? Почему этого не поймает ни один unit-тест?

<details><summary>Подсказка</summary>

Integration: «update rolls back when the closure fails» (`repository_suite_test.go:145`) — updateFn мутирует агрегат и возвращает ошибку, тест перечитывает и требует старое состояние. Unit-уровень слеп: домен не знает о транзакциях, app-спай не имеет настоящего rollback-а. Это ровно тот случай, ради которого правило 43 требует rollback-тест для каждого транзакционного repo — и сама эта проверка «сломай и убедись, что упало» и есть test sabotage из правила 43.
</details>

### 7. Sabotage: убрать FOR UPDATE ●●
*Тип: sabotage. Глава 7.*

Удалите `FOR UPDATE` из SELECT-а в `update()` (`auction_pg_repository.go:183`). Какой тест упадёт и как изменится **характер** ошибок у проигравших горутин? Зачем в коде оставлен optimistic version-check, если есть лок?

<details><summary>Подсказка</summary>

Падает «race: 20 concurrent equal bids, exactly one winner» (`repository_suite_test.go:259`): без лока все 20 читают одну версию. Но lost update всё равно не случится — сработает вторая линия обороны: `WHERE id = $1 AND version = $17` вернёт 0 строк, и проигравшие получат «optimistic lock conflict» — **инфраструктурную** ошибку вместо чистого доменного `ErrBidBelowMinimum`. Belt-and-suspenders (комментарий на строке 225): лок даёт корректную последовательную семантику и человеческие ошибки, version-check страхует от мутаций в обход лока. Прогоните suite с `-race` и сравните вывод до/после.
</details>

### 8. Sabotage: выпилить case из маппера ●●
*Тип: sabotage. Глава 9.*

Удалите `case auction.AuctionClosed` из `mapDomainEvent` (`events_mapper.go:69`). Что произойдёт, **когда** и кто это заметит? Сравните с альтернативным дизайном «незнакомое событие просто пропускаем» — почему он хуже?

<details><summary>Подсказка</summary>

`default`-ветка паникует на первом же `Close` — внутри транзакции репозитория: rollback, аукцион не закрыт, воркер ретраит и паникует снова. Заметит integration-тест (`TestPostgresOutbox` / suite) и component-`auction_lifecycle_test`. «Тихий скип» хуже катастрофически: аукцион закрылся бы, но `AuctionClosedV1` не родилось — сага никогда не стартует, счёт не выставлен, и никто не увидит (тихая потеря из главы 9). Громкая паника против тихой потери денег — осознанный трейдофф закрытого множества `DomainEvent` (правило 12).
</details>

### 9. Ревью: float64 для скидок ●●
*Тип: обоснуй ревью. Глава 11.*

PR в billing вводит `DiscountPercent float64` и считает `amount * (1 - discount/100)`. Напишите отказ со ссылками на главы и предложите дизайн, совместимый с инвойсом Molot.

<details><summary>Подсказка</summary>

Глава 11: деньги — только целые minor units (`invoice/money.go`, `AmountMinor int64`); float даёт невоспроизводимое округление (0.1+0.2), а «потерянный цент» в биллинге — инцидент сверки. Образец уже в коде: `invoice/commission.go` считает комиссию целочисленно с явной политикой округления. Скидка — те же basis points (`int64`, сотые процента) + задокументированное направление округления (в чью пользу — бизнес-решение, не математическое). Правило 7: тип `Discount` — value object с конструктором, не голый int в струте.
</details>

### 10. Ревью: запрос к чужой схеме ●●
*Тип: обоснуй ревью. Главы 2, 3, 8.*

PR в billing добавляет в query JOIN на `auction.auctions` — «зачем нам событие, если данные в соседней схеме, это же один Postgres». Напишите отказ и перечислите оба легальных пути получить эти данные.

<details><summary>Подсказка</summary>

Правило 3 (fail CI): чужая схема — чужая модель; JOIN намертво сцепляет billing со структурой write-таблицы auction — её рефакторинг, шардирование или вынос контекста ломают billing молча. Легальные пути: (1) собственная проекция нужных полей по подписке на `auction-events` — как `bidder_profiles` в самом auction (`adapters/bidder_profiles_pg.go` — проекция **чужих** participant-событий); (2) sync-вызов через consumer-side интерфейс + фасад `service/`, если процесс синхронен по природе (глава 10 — так settlement зовёт auction). Выбор между ними — по допустимой staleness.
</details>

### 11. Воркер против снайпа ●●
*Тип: найди в коде. Главы 7, 12.*

Закрывающий воркер отсканировал кандидата, а за миллисекунду до его транзакции снайп-ставка продлила окно. Проследите весь путь: почему аукцион не закроется преждевременно и почему воркер не зациклится на ошибке?

<details><summary>Подсказка</summary>

Скан `DueForClosing` — lock-free и заведомо устаревающий (комментарий в `auction_pg_repository.go:252`). Истина устанавливается под `FOR UPDATE`: `Close` перечитывает агрегат и видит свежий `ends_at` → `ErrBiddingStillOpen` (`auction.go:245`). Хендлер `close_auction.go` маппит его в nil — «не дозрел» это скип, не сбой (тест `handlers_test.go:179`). Следующий тик воркера найдёт аукцион снова, когда тот реально истечёт. Заодно: почему этот же механизм бесплатно обслужил `ExtendBySeller` из части 1?
</details>

### 12. Sabotage: сломать монотонный guard проекции ●●●
*Тип: sabotage. Главы 8, 9.*

Уберите условие `ci.bid_count < $3` из `ApplyBid` (`catalog_pg_projection.go:70`). Какой тест упадёт? Опишите production-сценарий, который стал возможен: какая комбинация redelivery и порядка доставки покажет покупателям **уменьшившуюся** цену?

<details><summary>Подсказка</summary>

Падает `TestPostgresProjectionsAndReadModels` (`pg_integration_test.go:210`): там bid №1 применяется дважды, затем приходит stale bid — без guard-а второй прогон перезаписал бы цену задним числом. Сценарий: at-least-once Watermill ретраит пачку после сбоя → `BidPlaced(count=3, 1500€)` доставлен повторно **после** `BidPlaced(count=4, 1600€)` → каталог показывает 1500€ при реальных 1600€ → пользователь ставит «минимальную следующую» и ловит `bid-below-minimum`. Сравните с `classifyMiss`: почему «0 строк затронуто» — это три разных исхода, а не один?
</details>

### 13. Ревью: вернуть агрегат из команды ●●●
*Тип: обоснуй ревью. Главы 5, 8, 16.*

PR меняет `PlaceBidHandler.Handle` так, чтобы он возвращал `*auction.Auction` — «фронту нужна новая минимальная ставка, зачем второй запрос». Напишите развёрнутый ответ: чем это плохо, какова честная цена отказа и какие **два** легальных решения у задачи фронта.

<details><summary>Подсказка</summary>

Нарушения: правило 26 (команда не возвращает бизнес-данных), и хуже — наружу уезжает доменный тип: транспорт прирастает к агрегату, любой рефакторинг домена ломает API (правило 7 — своя модель на слой). Цена отказа честно существует: +1 запрос. Решения: (1) книжный путь — 204, фронт перечитывает `AuctionCard` (id у него уже есть, ставка идемпотентна по client-generated `BidID` — глава 16); (2) если p99 правда болит — query handler с UI-shaped ответом, вызываемый портом **после** команды: CQRS не запрещает «команда, затем запрос» в одном HTTP-вызове, он запрещает один код-путь на оба. И контрольный вопрос: что из агрегата утекло бы фронту, верни мы его целиком? (См. упражнение 1.)
</details>

### 14. Спроектируй: автоставка (proxy bidding) ●●●
*Тип: спроектируй. Главы 5, 7, 8.*

Бидер задаёт максимум, система ставит за него минимально необходимое и автоматически перебивает соперников до потолка. Спроектируйте: какие новые инварианты появляются у `Auction`? Что происходит при двух автоставках друг против друга — и в скольких транзакциях? Как не сломать `ErrLeaderCannotOutbidSelf` и анти-снайп?

<details><summary>Подсказка</summary>

Ключевые решения: (1) максимум — секрет уровня резервной цены: не в событиях, не в API-ответах (упражнение 1 — образец); (2) дуэль автоставок схлопывается **в одной** транзакции одного `PlaceBid`-вызова — детерминированная серия внутри агрегата (лидер по большему максимуму, цена = чужой максимум + increment), иначе N промежуточных событий и анти-снайп-продление на каждое; (3) `ErrLeaderCannotOutbidSelf` остаётся для ручных ставок, но автоставка лидера «спит», пока его не перебьют — это новый инвариант, не исключение из старого; (4) считается ли авто-перебитие снайп-ставкой для `TriggersAt` — бизнес-вопрос, который надо отнести стейкхолдеру, а не решить молча. Хранение максимумов: внутри агрегата (раздувает снапшот) или отдельная таблица в той же транзакции (как `auction_bids`)? Аргументируйте через «граница агрегата = граница транзакции» (глава 5).
</details>

### 15. Спроектируй: Vickrey-аукцион ●●●
*Тип: спроектируй. Главы 2, 5, 9, 17.*

Платформа хочет формат Vickrey: ставки запечатаны до закрытия, побеждает наибольшая, **платит — вторую цену**. Что меняется в домене, событиях и проекциях? Что остаётся? И главный вопрос: это новый агрегат, режим существующего или новый bounded context?

<details><summary>Подсказка</summary>

Слом глубже, чем кажется: «запечатанность» убивает публичный `BidPlacedV1` с суммой (контракт append-only → нужен `SealedBidPlacedV1` без суммы, правило 34), каталог теряет `current_price`/`minimal_next_bid` (проекция и спека под нож), `MinimalNextBid` и `ErrBidBelowMinimum` не имеют смысла, анти-снайп бессмыслен (перебивать нечего — ставки не видны), зато `runnerUpBid` внезапно из вспомогательного становится **ценообразующим** — и `ClosingResult.Winner.Amount` больше не равен платежу: в `AuctionClosedV1` появляется отдельная цена сделки, и billing выставляет счёт по ней (заметьте: billing **не меняется** — он и так берёт сумму из события, не из модели аукциона). Это аргумент за отдельный агрегат `SealedAuction` в том же контексте: язык тот же («лот», «молоток», «продавец»), но инварианты почти не пересекаются — общий код выйдет меньше, чем кажется (глава 17: эволюция добавлением, не мутацией). Прогоните идею через event storming, как в шаге 0 части 1: на каком событии ломается текущий поток?
</details>

---

## Итог книги в одном абзаце

Если вычесть из этой главы все детали, останется одно наблюдение. Фича из части 1 потребовала ~120 строк продакшен-кода, и **ни одна** из них не решала проблем, не относящихся к самой фиче: транзакции, гонки, доставка событий, авторизация, наблюдаемость и контракт ошибок были решены архитектурой заранее — один раз и для всех будущих фич. Упражнения из части 2 показывают обратную сторону той же монеты: каждую гарантию можно найти в конкретной строке, сломать конкретным саботажем и поймать тестом конкретного уровня. Архитектура — это не схемы в документах; это свойство кодовой базы отвечать на вопрос «где это решается?» одним местом, а на вопрос «что сломается?» — одним упавшим тестом.
