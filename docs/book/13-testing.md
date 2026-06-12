# Глава 13. Тесты как архитектура

## Зачем читать эту главу

Большинство команд приходит к тестированию одной из двух дорог. Первая: писать всё подряд — и получить suite, который прогоняется 40 минут, флакает через раз и всё равно пропускает регрессии. Вторая: не писать почти ничего — и через год бояться трогать собственный код. Дороги разные, причина одна: тесты не спроектированы, а накоплены — как вещи в кладовке, куда годами складывали «потом разберём».

Эта глава — о том, как спроектировать тестовую пирамиду так, чтобы она обеспечивала реальную защиту, не превращалась в обузу и не лгала о покрытии. На конкретных файлах Molot разберём пять уровней: что, почему и как именно устроено. А в середине главы будет настоящая детективная история — про тест, который висел десять секунд на системе, давно пришедшей в нужное состояние.

---

## Проблема

Если вы когда-нибудь слышали на ревью «это должен быть unit-тест, а не integration» — и на следующий день снова слышали то же самое, только с перепутанными ролями, — значит, в вашей команде нет таксономии, а есть терминологический туман. В этом тумане живут несколько классических ошибок.

**Перевёрнутая пирамида.** «Давайте покроем всё e2e — они же самые реалистичные». Звучит убедительно, особенно от человека, который ещё не ждал их прогона. В итоге 300 e2e-тестов, каждый прогон по 45 минут, каждый второй флакает из-за timing. Когда suite падает — непонятно где, когда зелёный — непонятно, что именно покрыто. E2E не заменяют unit-тесты; они дополняют их сверху, а не снизу.

**Тесты моков проверяют моки.** Когда каждый метод сервиса замокан и проверяется вызов `mock.AssertCalled(t, "CreateUser", ...)` — вы не тестируете бизнес-логику. Вы тестируете, не изменилась ли внутренняя последовательность вызовов. При любом рефакторинге тесты падают, даже если поведение не изменилось. Это не защита — это шум.

**Cleanup как изоляция.** `t.Cleanup(func() { db.Exec("DELETE FROM auctions") })` — один из самых популярных источников flaky tests в integration-уровне. Cleanup не атомарен, параллельные тесты мешают друг другу, а порядок teardown непредсказуем при `t.Parallel()`.

**`require` внутри `assert.Eventually`.** Самый коварный пункт списка — настолько, что ему отведён отдельный раздел в середине главы. Мы наступили на эту граблю в реальном коде, и история того, как её искали, поучительнее любого правила. Потерпите до раздела о граблях testify — там будет всё: симптом, ложные следы и находка.

---

## Таксономия: таблица убивает споры

Любой спор о том, «что считать unit-тестом», решается не вкусом и не стажем спорящих — таблицей. Если таблица одна, лежит в репозитории и все с ней согласны, дискуссия занимает десять секунд: показываешь строку.

| Уровень | Docker DB | Внешние системы | Бизнес-фокус | Моки | Тестируемый API | Build tag | Команда |
|---|---|---|---|---|---|---|---|
| **Unit (domain)** | нет | нет | да | нет (ноль) | Go package (black-box `_test`) | — | `make test` |
| **Unit (app)** | нет | нет | оркестрация | recording spies | Go package | — | `make test` |
| **Integration** | да | нет | нет | обычно нет | Go package (адаптеры) | `integration` | `make test-integration` |
| **Component** | да | нет | да | только внешние (PSP fake) | HTTP | `component` | `make test-component` |
| **E2E** | да | да (нет внешних в Molot) | да | нет | HTTP (только публичный) | `e2e` | `make test-e2e` |

Каждый тест в репозитории попадает ровно в одну строку. Если тест не попадает — что-то не так с тестом, а не с таблицей. Имена уровней едины везде: в пакетах, в Makefile, в CI-конфиге (правило 39 BOOK_AUDIT) — одно слово означает одно и то же в любом контексте.

**Почему именно пять уровней, а не три?** Разделение domain/app внутри «unit» важно: оно физически запрещает бизнес-логику в тестах оркестрации. Разделение integration/component разграничивает «правильно ли мы используем Postgres» (адаптеры) от «работает ли весь процесс» (HTTP сверху донизу). Смешайте их — и получите тест, который медленно и непонятно проверяет что-то среднее, то есть худшее из обоих миров.

---

## Уровень 1: Domain — инварианты без моков

### Принципы

Фундамент пирамиды. Domain-тест проверяет бизнес-инварианты агрегата — те corner cases, которые бизнес сформулировал явно и за которые с вас спросят. Правил всего три, но держатся они строго:

1. **Black-box**: пакет `auction_test`, не `auction`. Тест не знает о приватных полях — только о публичном API. Поймали себя на мысли «мне бы доступ к полю» — это не повод для хитрого хака, а сигнал: у агрегата отсутствует нужный getter или API спроектирован не так.
2. **Ноль моков**. Агрегат получает `now time.Time` параметром (глава 6 учебника) — поэтому детерминированность достигается фиксированными значениями времени, не моками clock. Внешних зависимостей в домене нет by construction — мокать попросту нечего.
3. **Фикстуры только через доменный API**. Никаких `Auction{Status: "closed", BidCount: 3}` вручную — только `auction.New(...)`, `a.PlaceBid(...)`, `a.Close(...)`. Фикстура, собранная в обход конструктора, умеет создавать состояния, которых в реальной жизни не бывает, — и тест начинает охранять фантазию.

### Как в Molot

Файл `internal/auction/domain/auction/fixtures_test.go` содержит фикстурный слой. Канонический агрегат создаётся через `listedAuction(t)`:

```go
// internal/auction/domain/auction/fixtures_test.go:135–146
func listedAuction(t *testing.T) *auction.Auction {
    t.Helper()
    a, err := auction.New(
        newAuctionID(t), newSellerID(t), testLot(t),
        eur(t, 1000), eur(t, 100), auction.NoReserve(),
        testWindow(t), testRules(t), t0,
    )
    if err != nil {
        t.Fatal(err)
    }
    a.PullDomainEvents() // start tests from a clean slate
    return a
}
```

Два момента, на которые стоит посмотреть. `t0` — пакетная константа `time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)`: все тесты живут в одном фиксированном моменте, никакого `time.Now()`. А `PullDomainEvents()` сбрасывает накопленные при создании события — каждый тест начинает с чистого листа, а не разбирается с «хвостом» от конструктора.

`eur(t, amount)` и `usd(t, amount)` строят `Money` через `auction.NewCurrency` и `auction.NewMoney` — тот же конструктор, что и продовый код. Фикстура не обходит валидацию; она идёт тем же путём, что и реальные данные.

Тест `TestAuction_PlaceBid` в `place_bid_test.go` — классический table-driven Go (22 кейса, подтест на каждый):

```go
// internal/auction/domain/auction/place_bid_test.go:11–249
func TestAuction_PlaceBid(t *testing.T) {
    t.Parallel()

    bidTime := t0.Add(time.Hour)

    tests := []struct {
        name    string
        setup   func(t *testing.T) (*auction.Auction, auction.Bidder)
        amount  int64
        money   func(t *testing.T) auction.Money
        now     time.Time
        wantErr error
    }{
        {
            name: "first bid at start price is accepted",
            setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
                return listedAuction(t), verifiedBidder(t)
            },
            amount: 1000, now: bidTime,
        },
        {
            name: "first bid below start price is rejected",
            setup: func(t *testing.T) (*auction.Auction, auction.Bidder) {
                return listedAuction(t), verifiedBidder(t)
            },
            amount: 999, now: bidTime, wantErr: auction.ErrBidBelowMinimum,
        },
        {
            name: "seller cannot bid on own auction",
            // ...
            amount: 1000, now: bidTime, wantErr: auction.ErrSellerCannotBid,
        },
        {
            name: "unverified bidder exactly at threshold is rejected (GTE)",
            // ...
            amount: 100_000, now: bidTime, wantErr: auction.ErrVerificationRequired,
        },
        {
            name: "guard order: closed auction wins over below-minimum",
            // ...
            amount: 1, now: t0.Add(26 * time.Hour), wantErr: auction.ErrAuctionNotOpen,
        },
    // ...
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()
            a, bidder := tt.setup(t)
            // ...
            bid, err := a.PlaceBid(newBidID(t), bidder, amount(t), tt.now)

            if tt.wantErr != nil {
                if !errors.Is(err, tt.wantErr) {
                    t.Fatalf("PlaceBid error = %v, want %v", err, tt.wantErr)
                }
                if a.BidCount() != prevCount {
                    t.Fatalf("rejected bid mutated bidCount: %d -> %d", prevCount, a.BidCount())
                }
                return
            }
            // ...
        })
    }
}
```

На что смотреть в этом листинге. `t.Parallel()` стоит и на функции, и на каждом подтесте — весь table-driven suite параллелен по умолчанию, и это не оптимизация ради галочки, а способ держать `make test` быстрым. Sentinel-ошибки проверяются через `errors.Is` — не строковым сравнением и не `assert.Equal`: текст ошибки имеет право меняться, контракт — нет. После отклонённой ставки тест убеждается, что `BidCount()` не изменился, — то есть охраняет не только ошибку, но и отсутствие побочных эффектов: отказ, после которого агрегат уже не тот, — худший вид отказа. Наконец, кейсы `"guard order: ..."` фиксируют порядок guard-условий внутри метода: если `ErrAuctionNotOpen` перестанет возвращаться раньше `ErrBidBelowMinimum`, клиент получит вводящее в заблуждение сообщение — и заметит это именно тест.

Отдельно про именование: кейсы — snake_case-фразы на языке бизнеса. Не `test_case_7` и не `TestCase_FirstBidBelowStartPrice`, а `"first bid below start price is rejected"`. Когда такой тест падает в CI, сообщение об ошибке читается как строка спецификации — дежурному не нужно открывать код, чтобы понять, какое правило нарушено.

> **Совет из практики.** Дописав тест, сломайте код: закомментируйте один guard в `PlaceBid` и прогоните suite. Не упало — тест охраняет не то, что вы думаете. Эта sabotage-проверка занимает тридцать секунд и отличает тест от талисмана на удачу.

---

## Уровень 2: App — оркестрация через recording spies

### Принципы

Этажом выше домена живут use case handlers, и тестируют они ровно одно: правильно ли обработчик оркеструет зависимости. Порядок вызовов, прокидывание значений, маппинг sentinel-ошибок в no-op. Бизнес-логики здесь нет — она в домене, и проверять её второй раз не нужно.

Инструмент уровня — recording spy: рукописная структура, которая реализует интерфейс зависимости и записывает факты вызовов. Минимально — что было вызвано и с какими аргументами; опционально — скриптованные ошибки для тестирования crash seams. Никаких mock-фреймворков с DSL на сотню методов: обычная структура со срезами.

И сразу о главной ловушке уровня: факт вызова — ещё не ассерт. «Метод `IssueInvoice` был вызван» ничего не охраняет, пока вы не проверили, с каким `invoiceID`, `amount` и `attempt` он был вызван. Проверка факта без содержимого — театр тестирования: декорации стоят, защиты нет.

И последнее правило: если в app-тесте появляется бизнес-ветвление — логика пролезла не в свой слой. Не дописывайте тест — переносите логику в домен.

### Как в Molot

Файл `internal/settlement/app/spies_test.go` содержит рукописные spies для трёх зависимостей settlement-обработчиков:

```go
// internal/settlement/app/spies_test.go:22–85
// sagaRepoSpy is an in-memory settlement.Repository with scripted
// failures and a pre-commit hook: enough to simulate every crash seam
// and commit race of §6.7 without Docker.
type sagaRepoSpy struct {
    mu        sync.Mutex
    byAuction map[settlement.AuctionID]settlement.Settlement

    updateErr   error  // scripted commit failure ("crash before commit")
    onUpdate    func() // runs under the lock before updateFn — a concurrent commit
    addCalls    int
    updateCalls int
}

func (r *sagaRepoSpy) Update(
    ctx context.Context,
    id settlement.AuctionID,
    updateFn func(ctx context.Context, s *settlement.Settlement) (*settlement.Settlement, error),
) error {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.updateCalls++
    if r.updateErr != nil {
        return r.updateErr
    }
    if r.onUpdate != nil {
        hook := r.onUpdate
        r.onUpdate = nil
        hook()
    }
    // ...
}
```

Присмотритесь: `sagaRepoSpy` — полноценная in-memory реализация репозитория, и три её дополнительных рычага — весь инструментарий для моделирования отказов:
- `updateErr` — скриптованный сбой коммита («процесс упал перед коммитом»);
- `onUpdate` — хук, запускаемый под локом до выполнения `updateFn` (моделирует конкурентный коммит из другой горутины);
- счётчики `addCalls`/`updateCalls` для проверки количества вызовов.

`auctionGatewaySpy` записывает каждый вызов в срез:

```go
// internal/settlement/app/spies_test.go:125–158
type auctionGatewaySpy struct {
    awards    []settlement.AuctionID
    confirms  []settlement.AuctionID
    markFails []markFailCall
    relists   []relistCall

    awardErr, confirmErr, markFailErr, relistErr error
    onMarkSaleFailed                             func() // race hook between effect and commit
}

func (g *auctionGatewaySpy) MarkSaleFailed(_ context.Context,
    auctionID settlement.AuctionID, reason settlement.FailureReason) error {
    g.markFails = append(g.markFails, markFailCall{auctionID: auctionID, reason: reason})
    if g.onMarkSaleFailed != nil {
        hook := g.onMarkSaleFailed
        g.onMarkSaleFailed = nil
        hook()
    }
    return g.markFailErr
}
```

Заметьте, что хук `onMarkSaleFailed` одноразовый — сработав, он обнуляется: гонка моделируется ровно один раз, без сюрпризов на повторных вызовах. Как эти spies работают в деле — `handlers_test.go`:

```go
// internal/settlement/app/handlers_test.go:71–104
t.Run("seam: crash after the insert before IssueInvoice", func(t *testing.T) {
    t.Parallel()
    f := newFixture(t)
    e := soldAuction(soldOpts{})

    // First delivery dies in the effect: the saga row exists, the
    // invoice does not.
    f.billing.issueErr = errors.New("billing is down")
    require.Error(t, f.handlers.OnAuctionClosed(ctx, e))
    assert.Equal(t, settlement.StateStarted, f.repo.mustGet(t, auctionIDOf(t, e)).State())

    // Redelivery continues from Started: issue → commit (§6.7).
    f.billing.issueErr = nil
    require.NoError(t, f.handlers.OnAuctionClosed(ctx, e))
    assert.Equal(t, settlement.StateAwaitingPayment, f.repo.mustGet(t, auctionIDOf(t, e)).State())
})
```

Прочитайте подтест как сценарий: биллинг упал — строка саги осталась в `Started`, инвойса нет; биллинг ожил — повторная доставка довела процесс до `AwaitingPayment`. Каждый «шов падения» саги (§6.7 ARCHITECTURE.md) покрыт таким отдельным подтестом: задать скриптованную ошибку, запустить, проверить промежуточное состояние, снять ошибку, убедиться, что redelivery довершила начатое. Никакого Docker, никакого Postgres — только in-memory spy и реальный доменный код, поэтому весь набор швов прогоняется за миллисекунды.

Обратите внимание и на выбор `require`/`assert` — он не случаен. `require.Error` и `require.NoError` — это gate: если первый вызов не вернул ошибку, продолжать бессмысленно, тест падает сразу. `assert.Equal` — проверка значений, где продолжение полезно: за один прогон видно все несоответствия, а не только первое. Запомните это различие — в истории про `assert.Eventually` оно сыграет главную роль.

---

## Уровень 3: Integration — один suite, две реализации

### Принципы

Спускаемся к базе. Integration-тесты отвечают на один-единственный вопрос: «Правильно ли мы используем Postgres?» Не «работает ли бизнес-логика» — она уже проверена этажами выше, — а «корректно ли наш адаптер обращается с SQL-семантикой репозитория»: транзакции, локи, откаты.

Ключевые правила:

**Один shared suite на все реализации.** В Molot каждый репозиторий имеет две реализации: in-memory и Postgres. Один и тот же suite тестирует обе — и тем самым становится контрактом. Если in-memory проходит suite, а Postgres — нет, значит, реализации разошлись по контракту, и чинить нужно реализацию, а не подгонять тест. Если обе проходят — у вас есть обоснованная уверенность в обеих.

**Изоляция уникальными данными, не cleanup.** Каждый тест создаёт агрегаты с уникальными `uuid.New()` ID и ассертит по конкретному ID, а не по длине коллекции. Cleanup запрещён: он не атомарен, мешает параллельным тестам и замедляет suite, не давая взамен настоящей изоляции.

**Rollback-тест обязателен.** Он убеждается, что если `updateFn` возвращает ошибку, Postgres откатывает транзакцию и старое состояние сохраняется. Без него вы в откат верите, а не знаете о нём.

**Race-тест обязателен.** `close(start)` — классический паттерн: все горутины ждут закрытия канала и стартуют одновременно, обеспечивая максимальное давление на конкурентность. Гонки, которые «не воспроизводятся», — это обычно гонки, которым не устроили очную ставку.

### Как в Molot

`internal/auction/adapters/repository_suite_test.go` — shared suite, который принимает фабрику репозитория:

```go
// internal/auction/adapters/repository_suite_test.go:29–30
func runRepositorySuite(t *testing.T, newRepo func(t *testing.T) testRepository) {
    t.Helper()
```

In-memory тест вызывает его с `inmem`-реализацией, Postgres-тест — с pg-реализацией под `//go:build integration`.

Rollback-тест (`update rolls back when the closure fails`):

```go
// internal/auction/adapters/repository_suite_test.go:145–173
t.Run("update rolls back when the closure fails", func(t *testing.T) {
    t.Parallel()
    repo := newRepo(t)
    a := listedAuctionFx(t)
    if err := repo.Add(ctx, a); err != nil {
        t.Fatal(err)
    }
    sentinel := errors.New("sabotage")

    err := repo.Update(ctx, a.ID(), auction.ActorFromBidder(newBidderIDFx(t)),
        func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
            // Mutate first, then fail: nothing may leak out.
            if _, err := current.PlaceBid(newBidIDFx(t), verifiedBidderFx(t),
                eurFx(t, 1000), t0.Add(time.Hour)); err != nil {
                return nil, err
            }
            return nil, sentinel
        })
    if !errors.Is(err, sentinel) {
        t.Fatalf("err = %v, want the closure error", err)
    }

    got, err := repo.Get(ctx, a.ID())
    if err != nil {
        t.Fatal(err)
    }
    if got.BidCount() != 0 || got.Version() != 1 {
        t.Fatalf("rollback leaked: count=%d version=%d", got.BidCount(), got.Version())
    }
})
```

Смотрите на комментарий `Mutate first, then fail`: `updateFn` сначала честно мутирует агрегат — `PlaceBid` вызван, ставка добавлена — и только потом возвращает `nil, sentinel`. Падай closure до мутации, тест ничего бы не доказывал. А так проверка `BidCount() == 0`, `Version() == 1` — прямое подтверждение, что транзакция откатилась целиком и наружу не протекло ничего.

Race-тест с `close(start)`:

```go
// internal/auction/adapters/repository_suite_test.go:259–313
t.Run("race: 20 concurrent equal bids, exactly one winner", func(t *testing.T) {
    t.Parallel()
    repo := newRepo(t)
    a := listedAuctionFx(t)
    if err := repo.Add(ctx, a); err != nil {
        t.Fatal(err)
    }

    const bidders = 20
    start := make(chan struct{})
    winners := make(chan auction.BidID, bidders)
    var wg sync.WaitGroup

    for i := 0; i < bidders; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            bidder := verifiedBidderFx(t)
            bidID := newBidIDFx(t)
            <-start
            err := repo.Update(ctx, a.ID(), auction.ActorFromBidder(bidder.ID()),
                func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
                    if _, err := current.PlaceBid(bidID, bidder,
                        eurFx(t, 1000), t0.Add(time.Hour)); err != nil {
                        return nil, err
                    }
                    return current, nil
                })
            if err == nil {
                winners <- bidID
            } else if !errors.Is(err, auction.ErrBidBelowMinimum) {
                t.Errorf("loser got unexpected error: %v", err)
            }
        }()
    }
    close(start)
    wg.Wait()
    close(winners)

    var won []auction.BidID
    for id := range winners {
        won = append(won, id)
    }
    if len(won) != 1 {
        t.Fatalf("winners = %d, want exactly 1", len(won))
    }
    // ...
})
```

Двадцать горутин, одновременный старт через `close(start)`, одна и та же стартовая цена — двадцать претендентов на одну ставку. Проигравшие получают `ErrBidBelowMinimum` или конкурентную ошибку версионирования; в `winners` оказывается ровно один `BidID`, `BidCount() == 1`, `Version() == 2`. Любой другой расклад — баг репозитория, и тест его поймает.

Есть и более сложный race-тест — ставка против закрытия в снайп-окне. Проверяет, что один из двух исходов (bid или close победил) достигнут консистентно:

```go
// internal/auction/adapters/repository_suite_test.go:316–377
t.Run("race: close vs snipe bid — one consistent outcome", func(t *testing.T) {
    // ...
    switch {
    case bidErr == nil && errors.Is(closeErr, auction.ErrBiddingStillOpen):
        // Bid won: the window extended, the close was refused.
        if got.Status() != auction.StatusListed || got.BidCount() != 1 {
            t.Fatalf("bid-won state inconsistent: %v/%d", got.Status(), got.BidCount())
        }
    case closeErr == nil && errors.Is(bidErr, auction.ErrAuctionNotOpen):
        // Close won: the late bid was refused.
        if got.Status() != auction.StatusClosed || got.BidCount() != 0 {
            t.Fatalf("close-won state inconsistent: %v/%d", got.Status(), got.BidCount())
        }
    default:
        t.Fatalf("no consistent winner: closeErr=%v bidErr=%v", closeErr, bidErr)
    }
})
```

Этот тест гоняется с `-race` в CI. Его сила — в постановке вопроса: он не угадывает победителя, а перечисляет оба легальных исхода и объявляет ошибкой всё третье — оба упали, оба прошли, состояние несогласовано. Для конкурентного кода это единственный честный способ писать ассерты.

---

## Уровень 4: Component — весь процесс in-process

### Принципы

Поднимаемся на уровень целого приложения. Component-тест запускает всё in-process через composition root; мокается только внешний PSP. Его работа — happy path: всё собирается, миграции прошли, workers стартовали, HTTP-контракты отвечают. Corner cases сюда не носим — им место ниже, в domain и integration, где они на порядки дешевле.

Два composition root — для прода и для тестов — делегируют одной `newApplication`. Это принципиально: компонентный тест собирает тот же граф зависимостей, что и прод-бинарь, а не похожую на него декорацию.

### Как в Molot

`tests/component/main_test.go` — `TestMain` собирает приложение и ждёт readiness:

```go
// tests/component/main_test.go:80–127
app, err := monolith.NewComponentTestApplication(
    ctx, db, testHS256Secret, testPlatformCurrency,
    testDefaultPaymentTerm, testDefaultPSPMode,
)
// ...
sharedSrv = httptest.NewServer(app.HTTPHandler)
sharedClient = tests.NewClient(sharedSrv.URL, testHS256Secret)

// Poll /readyz until the watermill router is running (max 15 s).
if !waitForReadyz(sharedSrv.URL+"/readyz", 15*time.Second) {
    fmt.Fprintln(os.Stderr, "readyz did not return 200 within 15s")
    return 1
}
```

`waitForReadyz` — tight polling loop без `time.Sleep` как основы ожидания. Тест ждёт не N секунд, а факта готовности — разница, которая отделяет стабильный suite от флакающего.

Изолированная база данных создаётся через `createIsolatedDB` — уникальное имя на основе `uuid.NewString()`, создаётся перед тестами, дропается в teardown. Полная изоляция между прогонами без cleanup.

`TestAuctionLifecycle` — happy path, все участники, реальные JWT (не bypass):

```go
// tests/component/auction_lifecycle_test.go:96–111
// Poll variants only inside Eventually: require.* in the condition
// goroutine Goexits it and freezes Eventually (see tests/client.go).
assert.Eventually(t, func() bool {
    got, ok := c.AuctionCardPoll(t, auctionID, bidder1Token)
    return ok && got.Status == "closed"
}, 10*time.Second, 200*time.Millisecond,
    "auction was not closed by the worker within 10 s")
```

Комментарий над `assert.Eventually` ссылается на `tests/client.go` и паттерн `*Poll` — и за этой короткой ссылкой стоит история, из-за которой паттерн вообще появился. Время рассказать её целиком.

---

## Грабля testify: `require` внутри `assert.Eventually`

Это реальный баг из работы над Molot, и рассказать его стоит как историю — потому что история запоминается, а правило из стайлгайда забывается к пятнице.

Симптом выглядел так. Компонентный тест ждал, пока воркер закроет аукцион: `assert.Eventually` с бюджетом в десять секунд и опросом каждые 200 миллисекунд. Тест стабильно выедал все десять секунд и падал с `"Condition never satisfied"`. При этом по логам приложения всё было в порядке: воркер отработал, проекция сошлась, карточка отдавала `closed` уже на первых сотнях миллисекунд. Система приходила в нужное состояние почти мгновенно — а тест, который именно этого состояния и ждал, его в упор не видел.

Первая гипотеза — самая дешёвая и потому самая популярная: «не успевает, давайте увеличим таймаут». Увеличили. Тест стал висеть дольше и падать с тем же сообщением. Стало только подозрительнее: если система сходится за две сотни миллисекунд, какая разница — ждать десять секунд или тридцать?

Вторая гипотеза: сломан сам опрос — не тот URL, не тот токен, не тот статус в ответе. Проверка руками — тот же GET, тот же токен — возвращала `closed` со второй попытки. То есть условие, которое проверял `Eventually`, было истинным почти всё время ожидания. А `Eventually` упорно докладывал, что оно не выполнилось **ни разу**.

Вот это «ни разу» и оказалось ключом. Не «условие не успело стать истинным» — а «результат проверки ни разу не дошёл до Eventually». Осталось открыть исходники testify и посмотреть, как именно он запускает условие — и куда мог деваться результат.

### Механика зависания

Находка состоит из двух фактов, каждый из которых по отдельности известен любому Go-разработчику.

Факт первый: `assert.Eventually(t, condition, waitFor, tick, ...)` запускает `condition` в **отдельной горутине** через каждые `tick` и ждёт от неё `bool` — `true` или `false`.

Факт второй: `require.*` внутри тестов реализован через `t.FailNow()`, который в свою очередь вызывает `runtime.Goexit()`. А `runtime.Goexit()` немедленно завершает текущую горутину. Не тест — горутину. Включая горутину `condition`.

Теперь сложите факты. `require` срабатывает внутри условия — и горутина завершается, не отправив результат. `assert.Eventually` больше никогда не получит `true` от этой горутины (она мертва) и будет запускать новые горутины каждые `tick`... каждая из которых умрёт по той же причине. Итог: тест молча висит `waitFor` секунд и потом падает с `"Condition never satisfied"`, даже если система достигла нужного состояния за первые 50 миллисекунд. То самое «ни разу».

### Почему это особенно коварно при eventual consistency

Теперь понятно, почему ловушка целится именно в eventual-consistent системы — проекции, саги. Там 404 до конвергенции — не ошибка, а штатная фаза жизни: каждый ранний poll обязан его увидеть. Стоит написать так:

```go
// ОПАСНО: require внутри Eventually
assert.Eventually(t, func() bool {
    resp := c.do(t, "GET", "/api/auctions/"+id.String(), nil, token)
    require.Equal(t, http.StatusOK, resp.StatusCode) // <-- убивает горутину при 404
    var card AuctionCardResponse
    require.NoError(t, json.NewDecoder(resp.Body).Decode(&card))
    return card.Status == "closed"
}, 10*time.Second, 200*time.Millisecond)
```

— и первый же вызов, пока проекция не сошлась, получает 404. `require.Equal` срабатывает, `runtime.Goexit()` убивает горутину. `assert.Eventually` ждёт оставшиеся 10 секунд и выдаёт `"Condition never satisfied"`. Баг в тесте, а не в системе. Самое неприятное — снаружи это неотличимо от настоящей проблемы конвергенции, поэтому первой гипотезой и становится «увеличим таймаут».

> **Нюанс.** Это не каприз testify, а правило всей экосистемы Go: документация пакета `testing` прямо требует вызывать `t.FailNow()` из горутины самого теста. Любой хелпер, который под капотом делает FailNow — `require`, `t.Fatal`, `t.Fatalf`, — внутри чужой горутины превращается в тихую диверсию. `assert.Eventually` — просто самое популярное место, где это всплывает.

### Решение: `*Poll`-хелперы

Лечение — выгнать `require` из condition-горутины раз и навсегда, упаковав опрос в хелперы. Паттерн из `tests/client.go`:

```go
// tests/client.go:147–170
// pollJSON performs a GET and decodes the body into dst on 200, returning
// true. A non-200 returns false without failing the test (the read model
// has not converged yet). Transport/decode errors are reported non-fatally
// (assert, not require) so the surrounding assert.Eventually keeps control
// of its condition goroutine.
func (c *Client) pollJSON(t *testing.T, path, token string, dst any) bool {
    t.Helper()
    req, err := http.NewRequest(http.MethodGet, c.BaseURL+path, nil)
    if !assert.NoError(t, err, "pollJSON: build request %s", path) {
        return false
    }
    // ...
    resp, err := c.HTTPClient.Do(req)
    if !assert.NoError(t, err, "pollJSON: execute GET %s", path) {
        return false
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return false // not converged yet — let Eventually retry
    }
    b, err := io.ReadAll(resp.Body)
    if !assert.NoError(t, err, "pollJSON: read body %s", path) {
        return false
    }
    return assert.NoError(t, json.Unmarshal(b, dst),
        "pollJSON: decode %s: %s", path, string(b))
}
```

Три решения, на которых всё держится. Non-200 — это `return false`, а не `t.Fatal`: «ещё не готово» — нормальная фаза, не ошибка. Transport- и decode-ошибки — через `assert.NoError`, не `require`: если ошибка реальная, тест будет помечен как failed, но горутина доживёт до `return false`, и `assert.Eventually` сохранит контроль над своей condition-горутиной. И только после выхода из `assert.Eventually` в дело вступают `require`-хелперы — для пост-конвергентных проверок, где убивать уже некого.

```go
// tests/client.go:174–178
func (c *Client) AuctionCardPoll(t *testing.T, auctionID uuid.UUID, token string) (AuctionCardResponse, bool) {
    t.Helper()
    var card AuctionCardResponse
    ok := c.pollJSON(t, fmt.Sprintf("/api/auctions/%s", auctionID), token, &card)
    return card, ok
}
```

Использование:

```go
// tests/e2e/critical_path_test.go:116–119
assert.Eventually(t, func() bool {
    card, ok := c.AuctionCardPoll(t, auctionID, winnerToken)
    return ok && card.Status == "closed"
}, waitBudget, pollTick, "auction was not closed by the worker")
```

Из всей истории выжимается одно правило, которое стоит повесить в стайлгайд: внутри `assert.Eventually` — только `assert.*` и `*Poll`-хелперы; `require.*` — только снаружи, после того как `assert.Eventually` вернул управление. А мораль шире: когда тест врёт, читайте не только свой код, но и код инструмента, которым тестируете.

---

## Уровень 5: E2E — продовые бинари, контракты

Вершина пирамиды — самая дорогая и самая немногочисленная. E2E-тесты работают с prod-бинарями из `docker compose`, через публичный HTTP порт, и логику не проверяют вовсе — это работа domain-тестов. Их предмет — wiring: конфиг, миграции, SQL bus, workers, saga и HTTP stack, собранные точно как в проде. Поэтому падение e2e читается однозначно: кто-то сломал публичный контракт или порядок инициализации — и лучше узнать об этом от теста, чем от первого пользователя.

```go
// tests/e2e/critical_path_test.go:54–147
// TestCriticalPath drives the hammer-to-settlement flow end to end:
// register participants → operations verifies the bidders → seller lists a
// short-lived lot → two bids → ClosingWorker hammers the lot → the
// settlement saga issues an invoice → the winner pays → the saga settles.
func TestCriticalPath(t *testing.T) {
    baseURL := os.Getenv("E2E_BASE_URL")
    if baseURL == "" {
        t.Skip("E2E_BASE_URL not set; skipping e2e tests")
    }

    c := tests.NewClient(baseURL, devSecret)
    c.WaitForReadyz(t, waitBudget)

    // ... регистрация, верификация, листинг, ставки ...

    assert.Eventually(t, func() bool {
        card, ok := c.AuctionCardPoll(t, auctionID, winnerToken)
        return ok && card.Status == "closed"
    }, waitBudget, pollTick, "auction was not closed by the worker")
```

Два штриха, на которые стоит посмотреть. `waitBudget = 30 * time.Second`, `pollTick = 250 * time.Millisecond` — бюджет намеренно больше, чем в component-тестах (10 с, 200 мс): docker compose с prod-бинарями конвергирует медленнее, чем in-process httptest, и бюджеты это честно отражают. А скип при отсутствии `E2E_BASE_URL` — стандартный паттерн для уровней с Docker-зависимостью: без переменной тест молча пропускается, не превращая локальный прогон в стену красного.

> **Где вы на это наступите.** Тайминговые константы, подобранные на рабочем ноутбуке, — мина замедленного действия: CI-раннеры обычно слабее в разы, и «стабильные» локально секунды превращаются во флак на каждом третьем прогоне. Поэтому ожидание всегда формулируется как «поллинг до факта с запасом бюджета», а не «подождать N секунд», — и поэтому бюджеты component и e2e различаются осознанно, а не скопированы друг из друга.

---

## Трейдоффы

Как обычно, за каждое удобство уплачено. Главные размены:

**Скорость vs реализм.** Domain-тест — миллисекунды, e2e — десятки секунд. Делайте ставку на domain: каждый corner case, ушедший в e2e, стоит в 100–1000 раз дороже — и по времени прогона, и по времени диагностики.

**Shared suite vs изоляция.** Один suite на pg+inmem: тесты разных реализаций видят одну БД. Решение — уникальные ID, не разные базы. Дисциплина: ни один тест не ассертит по длине коллекции — только по конкретному ID.

**In-memory spy vs Docker в app-тестах.** Spy достаточен для тестирования оркестрации. Docker нужен только тогда, когда тестируется «правильно ли мы используем Postgres» — это integration-уровень.

**Cleanup vs уникальные данные.** Cleanup под `t.Parallel()` — источник хаоса: не атомарен, параллельные тесты мешают, порядок teardown непредсказуем. Уникальные данные изолируют без сайд-эффектов.

---

## Типичные ошибки

**`require` внутри `assert.Eventually`.** Вся история — выше. Симптом для распознавания: тест зависает на полный `waitFor` и падает с `"Condition never satisfied"`, хотя по логам система сошлась почти сразу.

**`time.Sleep` вместо поллинга.** `time.Sleep(5 * time.Second)` — ложь: на медленной машине мало, на быстрой — впустую. Правило: поллинг с deadline, никогда sleep как основное ожидание.

**Тест, который нельзя сломать.** Если баг в реализации не валит suite — тест не охраняет то, что думает. Sabotage-проверка: удалите один guard в `PlaceBid`, запустите suite. Не упало — тест отсутствует или написан неправильно.

**Ассерт по длине коллекции в integration-тесте.** `assert.Len(t, items, 3)` при параллельном прогоне — flaky в будущем: другой параллельный тест добавил четвёртый элемент. Ассертируйте по конкретным ID.

**Тест без `t.Parallel()`** в suite, где другие тесты параллельны. Один блокирующий тест превращает параллельный suite в последовательный. Обычная причина — cleanup или shared mutable state.

**Бизнес-ветвление в app-тесте.** `if auction.Status == "closed"` в тесте app-слоя — признак того, что логика просочилась из домена. App-тест знает о вызовах gateway и об ошибках из домена, не о бизнес-статусах.

---

## Чек-лист

При добавлении нового теста прогоните его по списку — это быстрее, чем разбирать флак через месяц:

- [ ] В какую строку таблицы таксономии попадает этот тест? Одна строка — один тест.
- [ ] Если это domain-тест: пакет `_test`, ноль моков, фикстуры через доменный API, `t.Parallel()` на функции и на каждом подтесте.
- [ ] Если это app-тест: spy записывает вызовы, тест проверяет содержимое (не только факт вызова), нет бизнес-ветвления.
- [ ] Если это integration-тест: shared suite, `t.Parallel()`, уникальные ID, нет cleanup, есть rollback-тест и race-тест.
- [ ] Если это component-тест: `NewComponentTestApplication` делегирует общему `newApplication`, readyz перед тестом, только happy path.
- [ ] Нет `time.Sleep` как основного ожидания — только поллинг с deadline.
- [ ] Нет `require.*` внутри `assert.Eventually` — только `assert.*` и `*Poll`-хелперы.
- [ ] Тест прогоняется с `-race`.
- [ ] Sabotage: намеренно сломал реализацию — тест упал? Если нет, тест не охраняет то, что должен.
- [ ] `make test` остаётся быстрым (цель < 10 с для unit-уровня).
- [ ] Тест добавлен в CI как merge-гейт.

---

## Ссылки

- `docs/test-taxonomy.md` — полная таблица таксономии, правила каждого уровня, бюджет прогона (правила 39–46 BOOK_AUDIT).
- `internal/auction/domain/auction/place_bid_test.go` — эталон table-driven domain-теста.
- `internal/auction/domain/auction/fixtures_test.go` — фикстуры только через доменный API.
- `internal/settlement/app/spies_test.go` — recording spies для app-уровня.
- `internal/settlement/app/handlers_test.go` — crash seam тесты саги.
- `internal/auction/adapters/repository_suite_test.go` — shared suite, rollback-тест, race-тест.
- `tests/component/main_test.go` — `TestMain` с `NewComponentTestApplication` и `waitForReadyz`.
- `tests/component/auction_lifecycle_test.go` — компонентный happy path.
- `tests/e2e/critical_path_test.go` — e2e critical path на prod-бинарях.
- `tests/client.go` — `*Poll`-хелперы и объяснение грабли `require` внутри `assert.Eventually`.
