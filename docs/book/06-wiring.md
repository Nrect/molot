# Глава 6. Соединение: consumer-side интерфейсы, clock, composition root

---

## Зачем читать эту главу

Предыдущие пять глав разобрали, *что* лежит внутри слоёв: агрегаты, репозитории, use cases, события. Остался вопрос, который возникает сразу после: **кто всё это соединяет и как именно?**

Вопрос кажется техническим — мол, как-нибудь соединим. Но три решения — где объявлять интерфейсы, как работать со временем и где инстанциировать конкретные типы — на практике определяют, можно ли тестировать систему без sleep, появятся ли циклы импортов через три месяца и сломается ли неправильная сборка на старте процесса или в три часа ночи на первом запросе. Ни у одного из трёх вопросов нет ответа «само получится» — поэтому глава и существует.

---

## Проблема: соединение разрушает архитектуру тихо

Слои разделены на бумаге. Но граница между ними сделана из конкретных типов, и как только один конкретный тип пересекает слой — начинается каскад: лишние импорты, за ними циклы, за ними «временные» решения, которые живут годами. Никто не принимает решение «сломать архитектуру» — она ломается десятком мелких уступок, каждая из которых по отдельности выглядела разумной.

Вот три самых распространённых способа убить чистую архитектуру при соединении. Присмотритесь — возможно, какой-то из них уже живёт в вашем проекте.

**Интерфейс объявляет реализация.** Пакет `adapters` выкладывает `IPaymentGatewayAdapter` рядом со своей структурой. App-слой вынужден импортировать `adapters`, чтобы использовать этот интерфейс. Стрелка зависимости развернулась — домен теперь смотрит в сторону инфраструктуры. Дальше хуже: интерфейс неизбежно растёт, потому что каждый новый метод адаптера добавляется в него автоматически, хочет того потребитель или нет.

**Время берётся напрямую.** `time.Now()` в методе агрегата или use case выглядит безобидно — ровно до задачи протестировать анти-снайпинг, экспирацию счёта или продление окна торгов. Тест, который зависит от реального времени, либо flakky, либо использует `sleep`, либо вообще не пишется. Целый класс поведения — как правило, самый денежный — остаётся непокрытым.

**Сборка размазана.** Конкретные типы инстанциируются по чуть-чуть везде: в пакете-хендлере, в `init()`, через глобальные переменные. Ошибка конфигурации обнаруживается не при запуске, а при первом запросе, который попадёт в нужный код-путь. DI-фреймворк с контейнером проблему не решает, а прячет: граф зависимостей скрыт за рефлексией, и ошибка сборки всё равно всплывает в рантайме, а не при чтении кода.

---

## Теория: три правила, которые решают всё

### Правило 1. Интерфейс объявляет потребитель

За образцом далеко ходить не надо — он в стандартной библиотеке. `io.Writer` объявлен в пакете `io` — там, где он *используется*. Не в пакете `os`, который предоставляет `*os.File`. Не в пакете `bytes`, который предоставляет `*bytes.Buffer`. Любая структура с методом `Write([]byte) (int, error)` молча удовлетворяет интерфейсу — без явного объявления, без импорта пакета-владельца интерфейса.

Для прикладного кода отсюда следуют четыре вещи, и все приятные. Интерфейс остаётся маленьким: потребитель объявляет ровно те методы, которые ему нужны, — не больше. Рукописный fake для теста — пара строк, моковые фреймворки не нужны. Циклов импортов не существует структурно: зависимость всегда направлена от реализации к потребителю, а не наоборот. И благодаря неявной реализации адаптер вообще не знает о существовании интерфейса потребителя — он просто реализует нужные методы.

Точная аналогия — список покупок: его пишет тот, кто будет готовить ужин, а не супермаркет. В супермаркете тысячи позиций (у адаптера — десятки методов), но в вашем списке три строки — те, что нужны сегодня.

### Правило 2. Время — зависимость, а не вызов

Агрегаты принимают `now time.Time` параметром в behavior-методах. App-слой получает текущее время через интерфейс `clock` с единственным методом `Now() time.Time`. В тесте подставляется `fixedClock{t}` — детерминированный, без sleep, без флака.

Не дайте простоте себя обмануть: это не «абстракция ради абстракции». Это граница между кодом, который можно тестировать без внешних зависимостей, и кодом, который нельзя.

### Правило 3. Composition root — одно место, один раз

Все конкретные типы встречаются ровно в одном месте — в composition root. В продакшн-бинаре это `cmd/monolith/main.go` + `internal/monolith/app.go`. В компонентных тестах — отдельная точка входа `NewComponentTestApplication`. Оба входа делегируют единственной приватной функции сборки.

Конструкторы паникуют на `nil`-зависимости — и неправильная сборка падает на старте процесса с читаемым сообщением, а не при первом запросе в продакшн. Думайте о composition root как об электрощитке квартиры: все автоматы в одном месте, выбило — идёте к щитку. Альтернатива — разводка, замурованная по всем стенам: обрыв будете искать с перфоратором.

Теперь — как все три правила выглядят в коде Molot.

---

## Как в Molot

### Consumer-side интерфейсы: три примера

**Auction, `place_bid.go`.** App-слой команды `PlaceBid` нуждается в двух зависимостях, помимо репозитория: профиль бидера и текущее время. Оба интерфейса объявлены приватно в том же пакете `command`, где они используются:

```go
// internal/auction/app/command/deps.go

// clock is the consumer-side time source (§10): aggregates take `now`
// as a parameter, handlers obtain it here — deterministic tests, no
// sleeps.
type clock interface {
    Now() time.Time
}

// bidderProfiles reads the local projection of participant events.
// Contract: a bidder without a profile row is an unverified bidder —
// the projection is eventually consistent and absence must not block
// small bids (the verification guard lives in the domain).
type bidderProfiles interface {
    BidderByID(ctx context.Context, id auction.BidderID) (auction.Bidder, error)
}
```

Один метод — один интерфейс. Адаптер, реализующий `bidderProfiles`, может содержать десятки методов для других нужд — потребителя это не касается. Тест подставляет `newSpyProfiles()` — рукописный spy из пяти строк. И обратите внимание на комментарий к `bidderProfiles`: краевой случай «нет строки профиля = неверифицированный бидер» прописан прямо у интерфейса — это решение, а не случайность, и читатель кода узнаёт его без раскопок.

**Billing, `pay_invoice.go`.** Интерфейс платёжного шлюза объявлен здесь же, в `command`:

```go
// internal/billing/app/command/pay_invoice.go

// paymentGateway is the consumer-side PSP port (§3.1). Contract:
//   - Charge is idempotent per key: a retry returns the original
//     reference without a second debit. A network failure surfaces as
//     invoice.ErrPSPUnavailable, a refusal as invoice.ErrPaymentDeclined.
//   - Refund is idempotent and a no-op when no charge exists for the
//     key — safe to call as crash-seam insurance.
type paymentGateway interface {
    Charge(ctx context.Context, key ChargeKey, amount invoice.Money) (invoice.PaymentReference, error)
    Refund(ctx context.Context, key ChargeKey) error
}
```

Два метода. Только то, что нужно команде `PayInvoice`. Вся семантика — в комментарии к интерфейсу: идемпотентность, коды ошибок, контракт повтора. Это и есть документ на адаптер — без отдельного wiki-раздела, который к тому же никто не обновляет.

**Settlement, `handlers.go`.** Сага реагирует на события трёх контекстов и вызывает синхронные фасады auction и billing. Интерфейсы объявлены в пакете `app`:

```go
// internal/settlement/app/handlers.go

// Consumer-side gateways (rule 5, §6, ADR-0004): the slice of the
// foreign sync facades the saga's event handlers need. Adapters bridge
// them to auctionservice.Facade / billingservice.Facade; every command
// is idempotent on the far side ("already in the target state" → nil),
// which makes the effect phase safely repeatable.
type auctionGateway interface {
    AwardToRunnerUp(ctx context.Context, auctionID settlement.AuctionID) error
    Relist(ctx context.Context, originalID, newID settlement.AuctionID, startsAt, endsAt time.Time) error
    MarkSaleFailed(ctx context.Context, auctionID settlement.AuctionID, reason settlement.FailureReason) error
    ConfirmSettlement(ctx context.Context, auctionID settlement.AuctionID) error
}

type billingGateway interface {
    IssueInvoice(ctx context.Context, invoiceID settlement.InvoiceID, auctionID settlement.AuctionID,
        debtor settlement.BidderID, amount settlement.Money, attempt int) error
}
```

`auctionGateway` — четыре метода. `billingGateway` — один. Реальный фасад `auctionservice.Facade`, который реализует `auctionGateway`, публичный и может раскрывать гораздо больше возможностей. Сага знает только те операции, которые ей нужны, — свой узкий срез чужого контекста.

Заметьте: settlement не импортирует `auctionservice` напрямую. Он объявляет интерфейс у себя. Адаптер в пакете `adapters` settlements берёт `*auctionservice.Facade` и реализует `auctionGateway`. Это единственная точка, где settlement знает о существовании auctionservice.

### Время как зависимость: детерминированный анти-снайп

С интерфейсами разобрались — теперь время, второе правило. В домене `auction` метод `PlaceBid` агрегата принимает `now time.Time` параметром. Логика анти-снайпинга описана в `antisnipe.go`:

```go
// internal/auction/domain/auction/antisnipe.go

// TriggersAt reports whether a bid placed at now is inside the snipe
// window of the endsAt deadline (now >= endsAt-window).
func (p AntiSnipePolicy) TriggersAt(now, endsAt time.Time) bool {
    return !now.Before(endsAt.Add(-p.window))
}
```

Нет `time.Now()`. Нет глобального состояния. Функция чистая — детерминированная для любого входа, и тестировать её можно как таблицу умножения.

В хендлере `PlaceBidHandler` время приходит через `clock`:

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

Смотрите на строку с `h.clock.Now()`: момент «сейчас» фиксируется один раз и передаётся в домен как факт — агрегат не дёргает часы сам. В тесте `h.clock.Now()` возвращает фиксированное значение, и весь сценарий анти-снайпинга — включая граничные условия, cap по числу продлений и гонку «ставка против закрытия» — покрыт детерминированными юнит-тестами. Ни одного `sleep` во всём тестовом наборе:

```go
// internal/auction/domain/auction/antisnipe_test.go

t.Run("bid inside snipe window extends the deadline", func(t *testing.T) {
    t.Parallel()
    a := listedAuction(t)
    placeBid(t, a, verifiedBidder(t), eur(t, 1000), end.Add(-3*time.Minute))
    if got, want := a.EndsAt(), end.Add(10*time.Minute); !got.Equal(want) {
        t.Fatalf("endsAt = %v, want %v", got, want)
    }
})

t.Run("close at old deadline fails after extension", func(t *testing.T) {
    t.Parallel()
    a := listedAuction(t)
    placeBid(t, a, verifiedBidder(t), eur(t, 1000), end.Add(-time.Minute))
    // The worker scanned before the extension: closing at the old
    // deadline must be refused (§10 race).
    if _, err := a.Close(end); !errors.Is(err, auction.ErrBiddingStillOpen) {
        t.Fatalf("err = %v, want ErrBiddingStillOpen", err)
    }
    if _, err := a.Close(end.Add(10 * time.Minute)); err != nil {
        t.Fatalf("close at the extended deadline: %v", err)
    }
})
```

Первый тест проверяет, что ставка за три минуты до дедлайна (при окне пять минут) сдвигает `EndsAt` на десять минут вперёд. Второй — гонку: воркер пытается закрыть аукцион по старому дедлайну, но ставка уже продлила окно — `ErrBiddingStillOpen` возвращается корректно. Оба теста мгновенные, без внешних зависимостей, и оба двигают время простой арифметикой над `end` — никакого ожидания настоящих минут.

> **Где вы на это наступите.** Тест, завязанный на реальное время, умеет проходить полгода и упасть в самый неудобный момент: на перегруженном CI-раннере, где между arrange и act прошла лишняя секунда; на границе суток в UTC; в ночь перевода часов. Хуже всего, что после ре-рана он снова зелёный — и команда привыкает жать retry вместо того, чтобы чинить. Один `fixedClock` дешевле, чем культура «перезапусти, оно иногда падает».

Аналогично в `PayInvoiceHandler`: `clock` инжектируется конструктором, тест передаёт `fixedClock` и проверяет все одиннадцать ветвей протокола оплаты, включая гонку между `MarkPaid` и экспирацией счёта.

> **Совет из практики.** Не полагайтесь на договорённость «в домене time.Now() не зовём» — договорённости не переживают ротацию команды. Заведите линт (forbidigo, ruleguard — что угодно), который запрещает `time.Now()` и `uuid.New()` в доменных и app-пакетах. Десять минут на настройку — и правило перестаёт зависеть от внимательности ревьюера в пятницу вечером.

### Composition root: два входа, один граф

Осталось третье правило — место, где всё это сходится. `internal/monolith/app.go` — единственный файл, где конкретные типы встречаются друг с другом. Комментарий пакета явно документирует это:

```go
// internal/monolith/app.go

// Package monolith contains the shared application assembly used by both
// the production entry point (cmd/monolith/main.go) and the component-test
// composition root (tests/component). The split follows BOOK_AUDIT rule 44:
// NewApplication (prod) and NewComponentTestApplication (test) each delegate
// to the single private newApplication, differing only in which adapters they
// supply.
```

Комментарий — не украшение: он фиксирует контракт «два входа, одна сборка», на который ниже опираются обе точки входа. Продакшн-точка инициализирует OTel-провайдеры, подключается к Postgres, передаёт их в `newApplication`:

```go
// internal/monolith/app.go

func NewApplication(
    ctx context.Context,
    cfg config.Config,
    logger *slog.Logger,
    db *sql.DB,
) (*Application, func(context.Context), error) {
    tracerProvider, err := tracing.NewTracerProvider(ctx, cfg.OTELExporterOTLPEndpoint)
    // ... инициализация meterProvider, authMiddleware ...

    app, err := newApplication(ctx, cfg, logger, db, authMiddleware, meterProvider, tracerProvider)
    // ...
    return app, shutdown, nil
}
```

Компонентный тест заменяет OTel-провайдеры на no-op и управляет PSP-режимом через параметр:

```go
// internal/monolith/app.go

func NewComponentTestApplication(
    ctx context.Context,
    db *sql.DB,
    hs256Secret string,
    platformCurrency string,
    paymentTerm time.Duration,
    pspMode string,
) (*Application, error) {
    // ...
    return newApplication(ctx, cfg, logger, db, authMiddleware,
        metricnoop.NewMeterProvider(), tracenoop.NewTracerProvider())
}
```

Сигнатура говорит сама за себя: тесту нужны база и горстка бизнес-параметров — всё остальное собирается ровно тем же путём, что и в проде. Оба входа делегируют `newApplication` — приватной функции, которая содержит весь граф:

```go
// internal/monolith/app.go

func newApplication(
    ctx context.Context,
    cfg config.Config,
    logger *slog.Logger,
    db *sql.DB,
    authMiddleware func(http.Handler) http.Handler,
    meterProvider metric.MeterProvider,
    tracerProvider trace.TracerProvider,
) (*Application, error) {
    // --- migrations ---
    // --- watermill ---
    // --- HTTP router ---

    // --- context assembly ---
    auctionSvc, err := auctionservice.NewService(auctionservice.Deps{
        DB:                  db,
        Logger:              logger,
        MeterProvider:       meterProvider,
        // ...
    })
    if err != nil {
        return nil, fmt.Errorf("assemble auction context: %w", err)
    }
    auctionSvc.RegisterHTTP(apiRouter)

    // ... billingSvc, settlementSvc, participantSvc, notificationSvc ...

    settlementSvc, err := settlementservice.NewService(settlementservice.Deps{
        DB:            db,
        AuctionFacade: auctionSvc.Facade(),
        BillingFacade: billingSvc.Facade(),
        // ...
    })
```

Весь граф — в одной функции, читается сверху вниз: сначала инфраструктура, потом каждый контекст, в конце — воркеры. Никакой магии. И обратите внимание на строку `AuctionFacade: auctionSvc.Facade()` — межконтекстные связи видны прямо здесь, явным присваиванием, а не где-то в недрах контейнера.

Конструкторы используют panic на nil-зависимостях. Из `place_bid.go`:

```go
// internal/auction/app/command/place_bid.go

func NewPlaceBidHandler(repo auction.Repository, profiles bidderProfiles, clock clock) PlaceBidHandler {
    if repo == nil {
        panic("NewPlaceBidHandler: nil repo")
    }
    if profiles == nil {
        panic("NewPlaceBidHandler: nil profiles")
    }
    if clock == nil {
        panic("NewPlaceBidHandler: nil clock")
    }
    return PlaceBidHandler{repo: repo, profiles: profiles, clock: clock}
}
```

Три проверки на три зависимости — скучно, зато сообщение паники называет и конструктор, и забытое поле. То же самое в `NewEventHandlers` из `settlement/app/handlers.go`:

```go
// internal/settlement/app/handlers.go

func NewEventHandlers(
    repo settlement.Repository,
    auctions auctionGateway,
    billing billingGateway,
    relist settlement.RelistPolicy,
    clk clock,
) EventHandlers {
    if repo == nil {
        panic("NewEventHandlers: nil repo")
    }
    if auctions == nil {
        panic("NewEventHandlers: nil auction gateway")
    }
    if billing == nil {
        panic("NewEventHandlers: nil billing gateway")
    }
    if relist.IsZero() {
        panic("NewEventHandlers: zero relist policy")
    }
    if clk == nil {
        panic("NewEventHandlers: nil clock")
    }
    return EventHandlers{...}
}
```

Любая ошибка конфигурации — например, забытый `AuctionFacade: auctionSvc.Facade()` в `Deps` — падает при запуске с точным сообщением. Не при первом событии `AuctionClosed` под нагрузкой.

> **Нюанс.** Паника в конструкторе уместна ровно для одного класса проблем — ошибок программиста при сборке: nil-зависимость не появляется «иногда в проде», она либо есть в графе, либо нет. Ошибки окружения — недоступная БД, кривой конфиг из env — другое дело: их возвращают как error, потому что у вызывающего есть осмысленная реакция (упасть с кодом выхода, залогировать, ретраить подключение). Смешаете два класса — получите либо сервис, который паникует от мигнувшей сети, либо error-обвязку вокруг багов, которые никто никогда не «обработает».

### Почему не DI-фреймворк

Wire, fx, dig — все они работают на рефлексии или кодогенерации. Главная цена — граф зависимостей становится невидимым при чтении кода: нужно запустить генерацию или знать аннотации фреймворка, чтобы понять, что с чем соединено.

В Molot весь граф помещается в один читаемый файл. `cmd/monolith/main.go` содержит восемьдесят строк: конфигурация, сигналы, HTTP-сервер, порядок shutdown. `internal/monolith/app.go` — граф зависимостей. Любой разработчик открывает файл и видит всё.

Это не догма «фреймворки — зло». DI-фреймворк оправдан, когда ручная сборка становится болью: сотни сервисов, многоуровневые графы зависимостей, необходимость переключать реализации флагами окружения. BOOK_AUDIT правило 44 формулирует условие перехода прямо: `NewApplication`/`NewComponentTestApplication` сводятся к общему `newApplication`. Пока один файл справляется — фреймворк не нужен. Когда перестанет справляться, вы это заметите без посторонней помощи.

---

## Трейдоффы

Как обычно, ничего бесплатного — взвесим честно.

**Consumer-side интерфейсы — за и против.**

Плюс: минимальные интерфейсы, нет циклов, рукописные fakes проще моковых фреймворков. Минус: если одна и та же реализация нужна десяти потребителям, возникает дублирование интерфейсов. В Molot это решается через фасады контекстов: публичный тип `auctionservice.Facade` реализует несколько мелких consumer-side интерфейсов молча — каждый потребитель видит только свой срез, а реализация одна.

**Ручная сборка — за и против.**

Плюс: граф читается как код, ошибки статически проверяемы, нет зависимости от внешней библиотеки. Минус: при росте числа контекстов файл сборки растёт. Противоядие — структура `Deps` на каждый сервис: именованные поля, не позиционные аргументы, добавление нового параметра не сдвигает всё остальное.

**Panic в конструкторах — за и против.**

Плюс: ошибка конфигурации обнаруживается сразу. Минус: паника в библиотечном коде, которую вызывает сторонний код, — плохой тон. Здесь паники в конструкторах `NewXxxHandler` — это нарушение программного контракта (передан `nil`), аналогично `index out of range`. Выбор осознанный: ошибки сборки не должны быть обрабатываемыми.

---

## Типичные ошибки

**Интерфейс у реализации.** Пакет `postgres` экспортирует `AuctionRepositoryInterface`. App-слой импортирует `postgres` ради интерфейса. Стрелка зависимости перевернулась — тихо, без единого предупреждения компилятора. Решение: переместить интерфейс в доменный пакет или оставить его приватным в app-пакете.

**`time.Now()` в домене.** Метод агрегата вызывает `time.Now()` внутри. Тест не может управлять временем без патча глобального состояния. Тесты на анти-снайп, экспирацию, продление окна либо не пишутся, либо используют `sleep`. Решение: все behavior-методы принимают `now time.Time` параметром.

**`time.Now()` в app-слое без интерфейса.** Хендлер вызывает `time.Now()` напрямую вместо `h.clock.Now()`. Результат тот же: хендлер непроверяем без реального времени. Решение: `clock` инжектируется через конструктор, проверка на nil обязательна.

**Global singletons.** `var db *sql.DB` на уровне пакета, инициализируется в `init()`. Порядок инициализации пакетов в Go не детерминирован для сложных графов. Тест, который инициализирует пакет с синглтоном, может получить состояние от предыдущего теста. Решение: все зависимости — явные параметры конструкторов.

**`init()` с побочными эффектами.** `init()` регистрирует хендлеры, открывает соединения, запускает горутины. Такой код невозможно контролировать из теста и невозможно выключить. Решение: вся инициализация — в явных конструкторах, вызываемых из composition root.

**Сборка в нескольких местах.** Часть сервисов собирается в `main.go`, часть — в middleware-пакете, часть — в `init()`. Новый разработчик не может найти, где что создаётся. Ошибка конфигурации проявляется непредсказуемо. Решение: composition root — один файл, все конкретные типы — там.

---

## Чек-лист

- [ ] Все интерфейсы, используемые app-слоем, объявлены приватно в том же пакете, где используются.
- [ ] Ни один app-пакет не импортирует пакет `adapters` напрямую.
- [ ] Все behavior-методы агрегатов принимают `now time.Time` параметром, не вызывают `time.Now()`.
- [ ] Все app-хендлеры получают `clock` через конструктор; конструктор паникует на `nil`.
- [ ] Composition root — один файл (или один пакет с явным входом); нет конкретных типов снаружи.
- [ ] Два входа в сборку (prod/component-test) делегируют единственной общей функции.
- [ ] Нет `init()` с побочными эффектами; нет package-level синглтонов.
- [ ] Все конструкторы хендлеров паникуют на `nil`-зависимостях с читаемым сообщением.
- [ ] В тестовом наборе нет `time.Sleep`; все тайм-зависимые сценарии покрыты `fixedClock`.
- [ ] Интерфейсы насчитывают 1–3 метода; более широкий интерфейс — сигнал проверить, всё ли нужно потребителю.

---

## Ссылки

- **TEXTBOOK.md, глава 6** — «Как части соединяются: интерфейсы у потребителя, время — параметр, сборка — в одном месте»; золотое правило 6.
- **BOOK_AUDIT.md, разделы 5–6** — consumer-side интерфейсы, направление зависимостей, ручная сборка.
- **BOOK_AUDIT.md, правило 44** — два composition root (`NewApplication`/`NewComponentTestApplication`) сводятся к одному общему `newApplication`; component-тесты — happy path only, auth через реальный путь.
- `internal/monolith/app.go` — полный граф зависимостей монолита.
- `internal/auction/app/command/deps.go` — `clock` и `bidderProfiles`: минимальные consumer-side интерфейсы.
- `internal/billing/app/command/pay_invoice.go` — `paymentGateway` с контрактом идемпотентности.
- `internal/settlement/app/handlers.go` — `auctionGateway` и `billingGateway` саги.
- `internal/auction/domain/auction/antisnipe_test.go` — детерминированные тесты анти-снайпинга без sleep.
