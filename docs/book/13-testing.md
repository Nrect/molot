# Глава 13. Тесты как архитектура

## Зачем читать эту главу

Большинство команд подходит к тестированию одним из двух способов: либо пишут всё подряд — и получают suite, который прогоняется 40 минут, флакает через раз и всё равно пропускает регрессии; либо не пишут почти ничего — и потом боятся трогать код. Обе стратегии имеют общую причину: тесты не спроектированы, а накоплены.

Эта глава — о том, как спроектировать тестовую пирамиду так, чтобы она обеспечивала реальную защиту, не превращалась в обузу и не лгала о покрытии. На конкретных файлах Molot разберём четыре уровня: что, почему и как именно они устроены.

---

## Проблема

Если вы когда-нибудь слышали на ревью «это должен быть unit-тест, а не integration» — и на следующий день снова слышали то же самое, только с перепутанными ролями, — значит, в вашей команде нет таксономии, а есть терминологический туман. В этом тумане живут несколько классических ошибок.

**Перевёрнутая пирамида.** «Давайте покроем всё e2e — они же самые реалистичные». В итоге 300 e2e-тестов, каждый прогон по 45 минут, каждый второй флакает из-за timing. Когда suite падает — непонятно где, когда зелёный — непонятно, что именно покрыто. E2E не заменяют unit-тесты; они дополняют их сверху, а не снизу.

**Тесты моков проверяют моки.** Когда каждый метод сервиса замокан и проверяется вызов `mock.AssertCalled(t, "CreateUser", ...)` — вы не тестируете бизнес-логику. Вы тестируете, не изменилась ли внутренняя последовательность вызовов. При любом рефакторинге тесты падают, даже если поведение не изменилось. Это не защита — это шум.

**Cleanup как изоляция.** `t.Cleanup(func() { db.Exec("DELETE FROM auctions") })` — один из самых популярных источников flaky tests в integration-уровне. Cleanup не атомарен, параллельные тесты мешают друг другу, а порядок teardown непредсказуем при `t.Parallel()`.

**`require` внутри `assert.Eventually`.** Этот баг особенно коварен. Пауза здесь, чтобы объяснить механику — потому что мы с ним столкнулись в реальном коде.

---

## Таксономия: таблица убивает споры

Любой спор о том, «что считать unit-тестом», решается не вкусом — таблицей. Если таблица одна и все с ней согласны, дискуссии нет.

| Уровень | Docker DB | Внешние системы | Бизнес-фокус | Моки | Тестируемый API | Build tag | Команда |
|---|---|---|---|---|---|---|---|
| **Unit (domain)** | нет | нет | да | нет (ноль) | Go package (black-box `_test`) | — | `make test` |
| **Unit (app)** | нет | нет | оркестрация | recording spies | Go package | — | `make test` |
| **Integration** | да | нет | нет | обычно нет | Go package (адаптеры) | `integration` | `make test-integration` |
| **Component** | да | нет | да | только внешние (PSP fake) | HTTP | `component` | `make test-component` |
| **E2E** | да | да (нет внешних в Molot) | да | нет | HTTP (только публичный) | `e2e` | `make test-e2e` |

Каждый тест в репозитории попадает в ровно одну строку. Если тест не попадает — значит, что-то не так с тестом, а не с таблицей. Имена уровней едины везде: в пакетах, в Makefile, в CI-конфиге (правило 39 BOOK_AUDIT).

**Почему именно пять уровней, а не три?** Разделение domain/app внутри «unit» важно: оно физически запрещает бизнес-логику в тестах оркестрации. Разделение integration/component разграничивает «правильно ли мы используем Postgres» (адаптеры) от «работает ли весь процесс» (HTTP сверху донизу). Смешать их — значит получить тест, который медленно и непонятно тестирует что-то среднее.

---

## Уровень 1: Domain — инварианты без моков

### Принципы

Domain-тест проверяет бизнес-инварианты агрегата — corner cases, которые бизнес сформулировал явно. Три правила:

1. **Black-box**: пакет `auction_test`, не `auction`. Тест не знает о приватных полях — только о публичном API. Если вы думаете «мне нужен доступ к полю» — значит, у агрегата отсутствует нужный getter или неправильный API.
2. **Ноль моков**. Агрегат получает `now time.Time` параметром (глава 6 учебника) — поэтому детерминированность достигается фиксированными значениями времени, не моками clock. Внешних зависимостей в домене нет by construction.
3. **Фикстуры только через доменный API**. Никаких `Auction{Status: "closed", BidCount: 3}` вручную — только `auction.New(...)`, `a.PlaceBid(...)`, `a.Close(...)`.

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

`t0` — пакетная константа `time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)`. Все тесты используют фиксированное время; никакого `time.Now()`. `PullDomainEvents()` сбрасывает накопленные при создании события, чтобы каждый тест начинал с чистого листа.

`eur(t, amount)` и `usd(t, amount)` строят `Money` через `auction.NewCurrency` и `auction.NewMoney` — тот же конструктор, что и продовый код. Фикстура не обходит валидацию; она идёт тем же путём.

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

Несколько важных деталей:

- `t.Parallel()` на уровне функции и на уровне каждого подтеста. Весь table-driven suite параллелен по умолчанию.
- Sentinel-ошибки проверяются через `errors.Is` — не строковое сравнение, не `assert.Equal`.
- После отклонённой ставки проверяется, что `BidCount()` не изменился. Тест проверяет не только ошибку, но и отсутствие побочных эффектов.
- Кейсы `"guard order: ..."` проверяют порядок guard-условий внутри метода. Это важно: если `ErrAuctionNotOpen` возвращается раньше `ErrBidBelowMinimum`, то клиент не получит вводящее в заблуждение сообщение.

Именование кейсов — snake_case-фразы на языке бизнеса. Не `test_case_7` и не `TestCase_FirstBidBelowStartPrice` — а `"first bid below start price is rejected"`. Когда тест падает в CI, сообщение об ошибке читается как спецификация.

---

## Уровень 2: App — оркестрация через recording spies

### Принципы

App-слой (use case handlers) тестирует только одно: что обработчик правильно оркеструет зависимости. Порядок вызовов, прокидывание значений, маппинг sentinel-ошибок в no-op. Не бизнес-логику — она в домене.

Recording spy — рукописная структура, которая реализует интерфейс зависимости и записывает факты вызовов. Минимально: что было вызвано и с какими аргументами. Опционально: скриптованные ошибки для тестирования crash seams.

Тест мока сам по себе — не ассерт. «Метод `IssueInvoice` был вызван» — не ассерт, если вы не проверяете, с каким `invoiceID`, `amount` и `attempt`. Факт вызова без проверки содержимого — театр, а не тест.

Если в app-тесте появляется бизнес-ветвление — оно пролезло не в свой слой. Переносите в домен.

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

`sagaRepoSpy` — полноценная in-memory реализация репозитория с тремя дополнительными возможностями:
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

Как используются spies в `handlers_test.go`:

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

Каждый «шов падения» саги (§6.7 ARCHITECTURE.md) покрыт отдельным подтестом. Тест задаёт скриптованную ошибку, запускает обработчик, проверяет промежуточное состояние, снимает ошибку и проверяет, что повторная доставка корректно довершила транзакцию. Никакого Docker, никакого Postgres — только in-memory spy и реальный доменный код.

Обратите внимание на использование `require`/`assert`. `require.Error` и `require.NoError` — это gate: если первый вызов не вернул ошибку, тест сразу падает, продолжать бессмысленно. `assert.Equal` — это проверка значений, где продолжение полезно (увидим все несоответствия за один прогон).

---

## Уровень 3: Integration — один suite, две реализации

### Принципы

Integration-тесты отвечают на один вопрос: «Правильно ли мы используем Postgres?» Не «работает ли бизнес-логика» — а «корректно ли наш адаптер использует SQL-семантику репозитория».

Ключевые правила:

**Один shared suite на все реализации.** В Molot каждый репозиторий имеет две реализации: in-memory и Postgres. Один и тот же suite тестирует обе. Это делает suite контрактом: если in-memory реализация проходит suite, а Postgres — нет, значит, реализация отличается по контракту. Если обе проходят suite, у вас есть уверенность в обеих.

**Изоляция уникальными данными, не cleanup.** Каждый тест создаёт агрегаты с уникальными `uuid.New()` ID. Ассерты — по конкретному ID, а не по длине коллекции. Cleanup запрещён: он не атомарен, мешает параллельным тестам и замедляет suite без реальной изоляции.

**Rollback-тест обязателен.** Убеждается, что если `updateFn` возвращает ошибку, Postgres откатывает транзакцию и старое состояние сохраняется.

**Race-тест обязателен.** `close(start)` — классический паттерн: все горутины ждут закрытия канала и стартуют одновременно, обеспечивая максимальное давление на конкурентность.

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

`updateFn` мутирует агрегат — `PlaceBid` вызван, ставка добавлена. Потом возвращает `nil, sentinel`. Тест проверяет, что агрегат в репозитории не изменился: `BidCount() == 0`, `Version() == 1`. Для Postgres это прямое подтверждение того, что транзакция откатилась.

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

20 горутин, одновременный старт через `close(start)`, одна стартовая цена. Проигравший получает `ErrBidBelowMinimum` или конкурентную ошибку версионирования. В `winners` — ровно один `BidID`. `BidCount() == 1`, `Version() == 2`.

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

Этот тест запускается с `-race` в CI. Он не просто проверяет результат — он проверяет, что возможны ровно два корректных исхода, и любой третий вариант (оба упали, оба прошли, несогласованное состояние) — ошибка.

---

## Уровень 4: Component — весь процесс in-process

### Принципы

Component-тест запускает всё приложение in-process через composition root. Мокается только внешний PSP. Проверяет happy path: всё собирается, миграции прошли, workers стартовали, HTTP-контракты работают. Corner cases — ниже, в domain и integration.
Два composition root — для прода и для тестов — делегируют одной `newApplication`. Компонентный тест собирает ту же граф зависимостей, что и прод-бинарь.

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

`waitForReadyz` — tight polling loop без `time.Sleep` как основы ожидания. Тест ждёт не N секунд, а факта готовности.

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

Комментарий в коде ссылается на `tests/client.go` — там объяснён паттерн `*Poll`. Разберём это подробно.

---

## Грабля testify: `require` внутри `assert.Eventually`

Это реальный баг, который мы нашли в процессе работы над Molot. Механика важна, потому что он встречается в любом проекте, где есть eventual consistency.

### Механика зависания

`assert.Eventually(t, condition, waitFor, tick, ...)` запускает `condition` в **отдельной горутине** через каждые `tick`. Горутина возвращает `bool` — `true` или `false`.

`require.*` внутри тестов реализован через `t.FailNow()`, который в свою очередь вызывает `runtime.Goexit()`. `runtime.Goexit()` немедленно завершает текущую горутину — включая горутину `condition`. Горутина завершилась, не отправив результат. `assert.Eventually` больше никогда не получит `true` от этой горутины (она мертва), и будет запускать новые горутины каждые `tick`... каждая из которых умрёт по той же причине. Итог: тест молча висит `waitFor` секунд и потом падает с `"Condition never satisfied"`, даже если система достигла нужного состояния за первые 50 миллисекунд.

### Почему это особенно коварно при eventual consistency

В eventual-consistent системе (проекции, сага) каждый poll ожидает увидеть 404 пока состояние не сошлось. Если писать:

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

При первом вызове, пока проекция не сошлась, приходит 404. `require.Equal` срабатывает, `runtime.Goexit()` убивает горутину. `assert.Eventually` ждёт оставшиеся 10 секунд и выдаёт `"Condition never satisfied"`. Баг в тесте, а не в системе.

### Решение: `*Poll`-хелперы

Паттерн из `tests/client.go`:

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

Ключевые решения:
- Non-200 — `return false`, не `t.Fatal`. Это «ещё не готово», не ошибка.
- Transport и decode ошибки — `assert.NoError` (не `require`). Если ошибка реальная — тест помечается как failed, но горутина продолжает работу и возвращает `false`. `assert.Eventually` может продолжить.
- После выхода из `assert.Eventually` — использовать `require`-хелперы для пост-конвергентных проверок.

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

Правило простое: внутри `assert.Eventually` — только `assert.*` и `*Poll`-хелперы. `require.*` — только снаружи, после того как `assert.Eventually` вернул управление.

---

## Уровень 5: E2E — продовые бинари, контракты

E2E-тесты работают с prod-бинарями из `docker compose`, через публичный HTTP порт. Не проверяют логику — это работа domain-тестов. Проверяют wiring: конфиг, миграции, SQL bus, workers, saga и HTTP stack — точно как в проде. Если e2e падает — кто-то сломал публичный контракт или порядок инициализации.

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

`waitBudget = 30 * time.Second`, `pollTick = 250 * time.Millisecond` — бюджет намеренно больше, чем в component-тестах (10 с, 200 мс): docker compose с prod-бинарями конвергирует медленнее, чем in-process httptest.

Скип при отсутствии переменной окружения — стандартный паттерн для уровней с Docker-зависимостью. Без `E2E_BASE_URL` тест молча пропускается, не падает.

---

## Трейдоффы

**Скорость vs реализм.** Domain-тест — миллисекунды, e2e — десятки секунд. Делайте ставку на domain: каждый corner case, ушедший в e2e, стоит в 100–1000 раз дороже.

**Shared suite vs изоляция.** Один suite на pg+inmem: тесты разных реализаций видят одну БД. Решение — уникальные ID, не разные базы. Дисциплина: ни один тест не ассертит по длине коллекции — только по конкретному ID.

**In-memory spy vs Docker в app-тестах.** Spy достаточен для тестирования оркестрации. Docker нужен только тогда, когда тестируется «правильно ли мы используем Postgres» — это integration-уровень.

**Cleanup vs уникальные данные.** Cleanup под `t.Parallel()` — источник хаоса: не атомарен, параллельные тесты мешают, порядок teardown непредсказуем. Уникальные данные изолируют без сайд-эффектов.

---

## Типичные ошибки

**`require` внутри `assert.Eventually`.** Описан выше. Симптом: тест зависает на полный `waitFor` и падает с `"Condition never satisfied"`, хотя система уже сошлась.

**`time.Sleep` вместо поллинга.** `time.Sleep(5 * time.Second)` — ложь: на медленной машине мало, на быстрой — впустую. Правило: поллинг с deadline, никогда sleep как основное ожидание.

**Тест, который нельзя сломать.** Если баг в реализации не валит suite — тест не охраняет то, что думает. Sabotage-проверка: удалите один guard в `PlaceBid`, запустите suite. Не упало — тест отсутствует или написан неправильно.

**Ассерт по длине коллекции в integration-тесте.** `assert.Len(t, items, 3)` при параллельном прогоне — flaky в будущем: другой параллельный тест добавил четвёртый элемент. Ассертируйте по конкретным ID.

**Тест без `t.Parallel()`** в suite, где другие тесты параллельны. Один блокирующий тест превращает параллельный suite в последовательный. Обычная причина — cleanup или shared mutable state.

**Бизнес-ветвление в app-тесте.** `if auction.Status == "closed"` в тесте app-слоя — признак того, что логика просочилась из домена. App-тест знает о вызовах gateway и об ошибках из домена, не о бизнес-статусах.

---

## Чек-лист

При добавлении нового теста:

- [ ] В какую строку таблицы таксономии попадает этот тест? Одна строка — один тест.
- [ ] Если это domain-тест: пакет `_test`, ноль моков, фикстуры через доменный API, `t.Parallel()` на функции и на каждом подтесте.
- [ ] Если это app-тест: spy записывает вызовы, тест проверяет содержимое (не только факт вызова), нет бизнес-ветвления.
- [ ] Если это integration-тест: shared suite, `t.Parallel()`, уникальные ID, нет cleanup, есть rollback-тест и race-тест.
- [ ] Если это component-тест: `NewComponentTestApplication` → общий `newApplication`, readyz перед тестом, только happy path.
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
