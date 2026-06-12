# Глава 12. Время как триггер: закрытие по дедлайну без cron

## Зачем читать

Каждый аукцион должен закрыться ровно тогда, когда истёк его дедлайн. Каждый неоплаченный счёт — истечь ровно через payment term. Обе задачи кажутся тривиальными: «сделаем cron, поставим скрипт». Именно здесь и начинается боль — утечки данных, гонки, невоспроизводимые баги в три часа ночи.

В этой главе разобран другой подход: **воркер внутри бинара**, написанный как тонкий оркестратор поверх доменных команд. Никакого отдельного cron-демона. Никакого скрипта, который лезет в базу в обход домена. Идемпотентность — в агрегате, а не в воркере. Тесты — детерминированные, без sleep.

Весь разбор опирается на реальный код проекта Molot; ничего не придумано.

---

## Проблема: почему cron снаружи домена ломается

Классическая схема выглядит так: cron раз в минуту запускает SQL-скрипт, который обновляет строки напрямую, публикует события и на этом заканчивает. На первый взгляд — просто и понятно. На практике — три системных дефекта.

**Дефект 1: транзакция расщеплена между двумя участниками.** Скрипт делает `UPDATE auctions SET status='closed'`, потом отдельным вызовом публикует событие в очередь. Между этими двумя операциями процесс может упасть. Событие потеряно, статус в базе уже изменён — домен и шина рассинхронизированы. Outbox-паттерн решает это проблему, но только если запись в outbox происходит **в той же транзакции**, что и мутация агрегата. Скрипт снаружи это условие нарушает по самой своей природе.

**Дефект 2: обход доменных инвариантов.** Агрегат знает: если ставка продлила окно, закрытие раньше нового дедлайна невозможно. Скрипт этого не знает. Он пишет `WHERE ends_at <= now()` и закрывает то, что закрывать ещё нельзя. Анти-снайп-политика обнуляется.

**Дефект 3: деплой и тестируемость.** Cron-скрипт — отдельный артефакт деплоя: своя версия, своё расписание, своя конфигурация. Его поведение в тестах не проверяется вместе с доменом. Гонка между cron и пользовательской ставкой тестируется... никогда, потому что для этого нужен реальный планировщик.

---

## Теория: паттерн «лёгкий скан + команда на каждого»

Правильное решение строится из трёх элементов.

**Дедлайн — данное агрегата, не вычисление воркера.** Воркер не должен решать, истёк ли дедлайн. Он должен только найти кандидатов, у которых это уже произошло, и попросить домен разобраться. Логика «истёк ли дедлайн» живёт в guard-е агрегата, и именно там проверяется `now >= endsAt`.

**Скан без блокировок — только для нахождения кандидатов.** `SELECT id FROM ... WHERE ends_at <= now() AND status = 'listed'` — это не мутация, это подсказка. Список кандидатов может быть устаревшим. Это нормально: если аукцион уже закрыт или ещё не истёк — guard агрегата вернёт соответствующую ошибку, и воркер её проигнорирует. Блокировку на этапе скана не берём; contention возникает только при реальной записи.

**Partial index — скан без сканирования всей таблицы.** Чтобы этот SELECT был O(кандидаты), а не O(все аукционы), нужен индекс, отсекающий терминальные строки на уровне структуры хранения:

```sql
-- internal/auction/adapters/migrations/00001_auction_schema.sql
-- closing worker candidate scan
CREATE INDEX auctions_due_idx ON auction.auctions (ends_at) WHERE status = 'listed';
```

Аналогично для счетов:

```sql
-- internal/billing/adapters/migrations/00001_invoices.sql
-- Partial index for the expiry worker's due scan (§10).
CREATE INDEX invoices_due_idx ON billing.invoices (due_at) WHERE status = 'pending';
```

`WHERE status = 'listed'` / `WHERE status = 'pending'` — это предикат partial index. В него попадают только строки в нужном состоянии. Закрытые аукционы и оплаченные счета в индекс не входят совсем. Рост таблицы на производительности скана не сказывается.

**Идемпотентность — в guard агрегата, не в воркере.** Воркер не проверяет, закрыт ли уже аукцион. Он просто вызывает `CloseAuction{AuctionID}`. Handler вызывает `a.Close(now)`. Guard агрегата возвращает `ErrAlreadyClosed`. Handler маппит его в `nil`. Ни одного события в outbox не записано — потому что транзакция была откатана самим guard-ом, прежде чем что-то закоммитилось. Повторный вызов абсолютно безопасен.

---

## Как в Molot: ClosingWorker

Воркер закрытия аукционов находится в `internal/auction/ports/worker.go`. Его цикл прост до скучности:

```go
// internal/auction/ports/worker.go

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

	tracer       trace.Tracer
	tickDuration metric.Float64Histogram
	dueBacklog   metric.Int64Gauge
}

func (w *ClosingWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

func (w *ClosingWorker) tick(ctx context.Context) {
	ctx, span := w.tracer.Start(ctx, "worker/closing.tick")
	defer span.End()

	workerAttr := metric.WithAttributes(attribute.String("worker", "closing"))
	start := time.Now()
	defer func() {
		w.tickDuration.Record(ctx, time.Since(start).Seconds(), workerAttr)
	}()

	ids, err := w.scanner.DueForClosing(ctx, w.clock.Now(), closingCandidateLimit)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		w.logger.ErrorContext(ctx, "closing worker scan failed", slog.Any("error", err))
		return
	}
	span.SetStatus(codes.Ok, "")
	w.dueBacklog.Record(ctx, int64(len(ids)), workerAttr)

	for _, id := range ids {
		if err := w.closeAuction.Handle(ctx, command.CloseAuction{AuctionID: id}); err != nil {
			w.logger.ErrorContext(ctx, "closing auction failed",
				slog.String("auction_id", id.String()), slog.Any("error", err))
		}
	}
}
```

Обратите внимание на то, чего здесь **нет**: нет проверки статуса аукциона, нет `if alreadyClosed`, нет ретрая с задержкой, нет логики дедлайна. Воркер не знает ничего о домене — только то, что нужно вызвать `CloseAuction`. Все решения — в handler-е и агрегате.

Handler полностью выражает политику обработки ошибок:

```go
// internal/auction/app/command/close_auction.go

func (h CloseAuctionHandler) Handle(ctx context.Context, cmd CloseAuction) error {
	err := h.repo.UpdateAsSystem(ctx, cmd.AuctionID,
		func(ctx context.Context, a *auction.Auction) (*auction.Auction, error) {
			if _, err := a.Close(h.clock.Now()); err != nil {
				return nil, err
			}
			return a, nil
		})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, auction.ErrAlreadyClosed),
		errors.Is(err, auction.ErrBiddingStillOpen),
		errors.Is(err, auction.ErrAuctionCancelled):
		// The updateFn error rolled the transaction back; nothing was
		// persisted and no event was published. Benign for the worker.
		return nil
	default:
		err, _ = mapNotFound(err)
		return err
	}
}
```

Три sentinel-а превращаются в `nil`. Это не «проглатывание ошибок» — это явный контракт: для воркера эти исходы доброкачественны. `ErrBiddingStillOpen` особенно важен: это значит, что ставка успела продлить окно между сканом и `Close`. Воркер просто подождёт следующего тика.

## Как в Molot: ExpiryWorker

Воркер истечения счетов (`internal/billing/ports/worker.go`) устроен зеркально:

```go
// internal/billing/ports/worker.go

// ExpiryWorker is the payment-timeout worker (ARCHITECTURE.md §10): a
// ticker scans pending invoices past due_at and drives each through the
// ordinary ExpireInvoice command (UpdateAsSystem → guard table). The
// deadline itself is invoice data (due_at = issued_at + PAYMENT_TERM);
// the saga never sleeps — it only reacts to InvoiceExpiredV1.
type ExpiryWorker struct {
	due      dueInvoices
	expire   decorator.CommandHandler[command.ExpireInvoice]
	interval time.Duration
	clock    workerClock
	logger   *slog.Logger

	tracer       trace.Tracer
	tickDuration metric.Float64Histogram
	dueBacklog   metric.Int64Gauge
}

func (w *ExpiryWorker) tick(ctx context.Context) {
	ctx, span := w.tracer.Start(ctx, "worker/expiry.tick")
	defer span.End()

	workerAttr := metric.WithAttributes(attribute.String("worker", "expiry"))
	start := time.Now()
	defer func() {
		w.tickDuration.Record(ctx, time.Since(start).Seconds(), workerAttr)
	}()

	ids, err := w.due.PendingDueBefore(ctx, w.clock.Now(), expiryBatchLimit)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		w.logger.ErrorContext(ctx, "expiry worker: due scan failed",
			slog.String("context", "billing"), slog.Any("error", err))
		return
	}
	span.SetStatus(codes.Ok, "")
	w.dueBacklog.Record(ctx, int64(len(ids)), workerAttr)

	for _, id := range ids {
		if err := w.expire.Handle(ctx, command.ExpireInvoice{InvoiceID: id}); err != nil {
			w.logger.ErrorContext(ctx, "expiry worker: expire failed",
				slog.String("context", "billing"),
				slog.String("invoice_id", id.String()),
				slog.Any("error", err))
		}
	}
}
```

Разница в одном: `dueInvoices` — это consumer-side interface, отдельный от полного `Repository`. Воркер видит только `PendingDueBefore(ctx, t, limit)` — ни одного другого метода. Это намеренное сужение: воркер не может случайно написать логику поверх репозитория.

## Дедлайн как данное агрегата, не как расписание воркера

Принципиальный момент, который отличает этот дизайн от cron-подхода: **воркер не хранит дедлайны**. Дедлайн — атрибут агрегата.

Для счета: `due_at = issued_at + PAYMENT_TERM`. Это поле записывается при создании `Invoice` в домене и хранится в строке `billing.invoices`. Воркер лишь спрашивает: «какие pending-счета уже прошли свой due_at?» — и получает список готовых ID.

Для аукциона: `ends_at` хранится в строке агрегата и может быть продлён анти-снайп-политикой. Воркер спрашивает: «какие listed-аукционы имеют ends_at в прошлом?» — и получает кандидатов.

Сага при этом **не спит и не хранит таймеров**. Она реагирует на события: `InvoiceExpiredV1` прилетело — значит, воркер сделал своё дело. Saga не знает, когда именно истечёт срок; она только реагирует на факт истечения.

---

## Гонка PlaceBid против Close в снайп-окне

Это самая интересная гонка в системе, и она разрешается без координатора.

**Сценарий.** Воркер сканирует кандидатов в момент `T`. Аукцион попадает в список: его `ends_at = T - 1s`. Одновременно пользователь размещает ставку в последнюю секунду, которая попадает в анти-снайп-окно (5 минут до дедлайна) и продлевает `ends_at` на 10 минут. Два UPDATE идут к одной строке.

**Механизм.** Оба пути — `PlaceBid` и `Close` — проходят через `repo.UpdateAsSystem` / `repo.Update`, внутри которых `SELECT ... FOR UPDATE` на строку аукциона. Один из них захватит row lock первым.

*Сценарий A: ставка пришла первой.* `PlaceBid` захватывает lock, продлевает `ends_at`, коммитит. `Close` получает lock, перечитывает агрегат с новым `ends_at` и вызывает `a.Close(now)`. Guard агрегата проверяет: `now < endsAt` → возвращает `ErrBiddingStillOpen`. Handler маппит в nil. Аукцион не закрыт. Следующий тик подберёт его.

*Сценарий B: Close пришёл первым.* `Close` захватывает lock, закрывает аукцион, коммитит `AuctionClosedV1`. `PlaceBid` получает lock, перечитывает — статус `closed` → `ErrAuctionNotOpen` → HTTP 409. Опоздавшая ставка отклонена.

Именно этот сценарий закрыт тестом:

```go
// internal/auction/adapters/repository_suite_test.go

t.Run("race: close vs snipe bid — one consistent outcome", func(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a := listedAuctionFx(t)
	if err := repo.Add(ctx, a); err != nil {
		t.Fatal(err)
	}
	deadline := t0.Add(24 * time.Hour)
	bidder := verifiedBidderFx(t)

	start := make(chan struct{})
	var closeErr, bidErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		closeErr = repo.UpdateAsSystem(ctx, a.ID(),
			func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
				if _, err := current.Close(deadline); err != nil {
					return nil, err
				}
				return current, nil
			})
	}()
	go func() {
		defer wg.Done()
		<-start
		bidErr = repo.Update(ctx, a.ID(), auction.ActorFromBidder(bidder.ID()),
			func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
				// Inside the snipe window (5m before the deadline).
				if _, err := current.PlaceBid(newBidIDFx(t), bidder, eurFx(t, 1000), deadline.Add(-time.Minute)); err != nil {
					return nil, err
				}
				return current, nil
			})
	}()
	close(start)
	wg.Wait()

	got, err := repo.Get(ctx, a.ID())
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case bidErr == nil && errors.Is(closeErr, auction.ErrBiddingStillOpen):
		// Bid won: the window extended, the close was refused.
		if got.Status() != auction.StatusListed || got.BidCount() != 1 {
			t.Fatalf("bid-won state inconsistent: %v/%d", got.Status(), got.BidCount())
		}
		if !got.EndsAt().After(deadline) {
			t.Fatal("snipe bid must have extended the deadline")
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

Тест не знает, кто победит. Он проверяет, что любой из двух исходов внутренне консистентен. Это и есть корректность без координатора.

## Анти-снайп-политика как снапшот агрегата

Анти-снайп не читается из конфига при каждой ставке. Он снапшотится на агрегат в момент листинга и хранится в трёх колонках:

```sql
-- internal/auction/adapters/migrations/00001_auction_schema.sql
snipe_window_sec    int         NOT NULL,        -- anti-snipe policy snapshot
snipe_extension_sec int         NOT NULL,
snipe_max_ext       int         NOT NULL,
```

Доменный объект:

```go
// internal/auction/domain/auction/antisnipe.go

// AntiSnipePolicy is the platform anti-sniping rule snapshotted onto
// the aggregate at listing time (§2.1): a bid landing within `window`
// of the deadline extends it by `extension`, at most `maxExtensions`
// times. Snapshotting keeps a running auction's rules deterministic.
type AntiSnipePolicy struct {
	window        time.Duration
	extension     time.Duration
	maxExtensions int
}

// TriggersAt reports whether a bid placed at now is inside the snipe
// window of the endsAt deadline (now >= endsAt-window).
func (p AntiSnipePolicy) TriggersAt(now, endsAt time.Time) bool {
	return !now.Before(endsAt.Add(-p.window))
}
```

Тест проверяет граничный случай — именно то место, где воркер чаще всего ошибается при наивной реализации:

```go
// internal/auction/domain/auction/antisnipe_test.go

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

Снапшот гарантирует: смена платформенной политики анти-снайпа не влияет на уже идущие аукционы. Лот детерминирован во времени.

---

## Несколько реплик: почему гонка доброкачественна

Если запущены несколько реплик монолита, оба воркера закрытия могут получить одинаковый список кандидатов из скана (скан без блокировок — это нормально). Оба пытаются закрыть один и тот же аукцион.

Первый захватывает row lock, закрывает, коммитит. Второй захватывает lock, перечитывает агрегат — `status = 'closed'` → `ErrAlreadyClosed` → nil. Никаких дублирующих событий: outbox пишется только внутри той транзакции, которая реально изменила статус. Проигравшая транзакция была откатана.

Дополнительно работает optimistic lock: `UPDATE ... WHERE id = $1 AND version = $expected`. Это belt-and-suspenders поверх FOR UPDATE — если вдруг две транзакции прошли мимо блокировки (что теоретически невозможно при корректном FOR UPDATE, но страхует от ошибок реализации), UPDATE одной из них затронет 0 строк и попадёт в ветку ошибки.

Если contention реплик станет измеримым (сигнал — метрика `molot_worker_lock_wait`), правильная мера — случайная фаза тикера на реплику через `CLOSING_POLL_JITTER`. Это разнесёт скан во времени и уменьшит вероятность коллизий. `SKIP LOCKED` в кандидатном скане неприменим: скан безлоковый по замыслу, а корректность держат guard-ы агрегата, а не блокировки в скане.

---

## Запуск в errgroup: воркер — полноправный горутин монолита

Оба воркера запускаются из одного `errgroup` вместе с HTTP-сервером и Watermill-роутером:

```go
// internal/monolith/app.go

runWorkers := func(runCtx context.Context) error {
	g, runCtx := errgroup.WithContext(runCtx)

	g.Go(func() error {
		if err := wmRouter.Run(runCtx); err != nil {
			return fmt.Errorf("watermill router: %w", err)
		}
		return nil
	})

	g.Go(auctionSvc.ClosingWorker(runCtx))
	g.Go(billingSvc.ExpiryWorker(runCtx))

	return g.Wait()
}
```

`Run` воркера возвращает `nil` при отмене контекста — shutdown чистый, не отравляет errgroup. Сигнал `SIGTERM` приходит в `signal.NotifyContext`, отменяет верхний ctx, errgroup получает сигнал, все горутины останавливаются в порядке отмены.

---

## Наблюдаемость воркеров

Каждый тик воркера оборачивается span-ом. Из кода `ClosingWorker.tick`:

```go
ctx, span := w.tracer.Start(ctx, "worker/closing.tick")
defer span.End()
```

Из этого span-а дочерними становятся `commands/CloseAuction` — один на каждый аукцион в батче. Итог: в трейсе видно, сколько аукционов закрыл один тик, какой из них занял больше всего времени.

Метрики:
- `molot_worker_tick_duration{worker="closing"}` — гистограмма длительности тика. Рост p99 означает, что батч вырос или БД перегружена.
- `molot_worker_due_backlog{worker="closing"}` — количество кандидатов, найденных в последнем тике. Постоянный рост — воркер не успевает обработать батч за интервал тикера.

Алерт по `backlog > threshold` — это и есть сигнал, что интервал тикера нужно уменьшить или параллелизм увеличить.

---

## Тестируемость: часы как параметр

Воркер принимает `clock interface{ Now() time.Time }`. Агрегаты принимают `now time.Time` как явный параметр. Нет ни одного вызова `time.Now()` в доменном или app-слое.

Тест воркера проверяет оркестрацию, не домен:

```go
// internal/auction/ports/worker_test.go

func TestClosingWorker_ClosesScannedCandidates(t *testing.T) {
	t.Parallel()

	id, err := auction.NewAuctionID(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	handler := recordingCloseHandler{got: make(chan command.CloseAuction, 1)}
	scanner := &stubScanner{ids: []auction.AuctionID{id}}
	clock := stubClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}

	worker := ports.NewClosingWorker(handler, scanner, clock, time.Millisecond, slog.Default(),
		tracenoop.NewTracerProvider(), metricnoop.NewMeterProvider())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	select {
	case cmd := <-handler.got:
		if cmd.AuctionID != id {
			t.Fatalf("closed %v, want %v", cmd.AuctionID, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker never issued CloseAuction")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("worker returned %v on clean shutdown, want nil", err)
	}
}
```

Тест не тестирует «правильно ли закрылся аукцион» — это домен. Он тестирует: «отправил ли воркер CloseAuction с правильным ID». Разделение ответственности в действии.

Тесты анти-снайпа полностью детерминированы — никаких sleep, никакого реального времени:

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
```

`end.Add(-3*time.Minute)` — просто `time.Time`, переданный в `PlaceBid`. Тест запускается за наносекунды.

---

## Трейдоффы

| Решение | Плюс | Минус |
|---|---|---|
| Воркер в бинаре | Outbox атомарен с мутацией; тест с реальным доменом; один деплой | Если сервис упал — воркер тоже упал; нужна горизонтальная устойчивость |
| Partial index | O(кандидаты) без роста с таблицей | Нужна дисциплина: при добавлении нового состояния — проверить, не должен ли индекс его включить |
| Идемпотентность в агрегате | Воркер «тупой», любой вызов безопасен | Guard-ы нужно тщательно проектировать и покрывать тестами — случайный no-op опасен |
| Несколько реплик | Нет единой точки отказа | Гонки реплик потребляют row lock; при высоком TPS может потребоваться jitter |
| `ErrBiddingStillOpen` → nil | Опоздавший Close — не ошибка, нормальное течение | Нельзя использовать этот sentinel для детектирования реальных проблем |

---

## Типичные ошибки

**1. Sleep-цикл вместо тикера с ctx.**

```go
// Неправильно
for {
    processExpired()
    time.Sleep(5 * time.Second) // контекст не проверяется
}

// Правильно
ticker := time.NewTicker(w.interval)
defer ticker.Stop()
for {
    select {
    case <-ctx.Done():
        return nil
    case <-ticker.C:
        w.tick(ctx)
    }
}
```

Sleep-цикл не останавливается при отмене контекста. Shutdown превращается в гонку.

**2. Удаление до проверки.**

```go
// Неправильно: закрыли аукцион, потом опубликовали событие отдельно
db.Exec("UPDATE auctions SET status='closed' WHERE ...")
bus.Publish(AuctionClosedEvent{...})

// Правильно: оба в одной транзакции через outbox
repo.UpdateAsSystem(ctx, id, func(ctx context.Context, a *Auction) (*Auction, error) {
    if _, err := a.Close(now); err != nil { return nil, err }
    return a, nil
    // AuctionClosed домен записал в events;
    // repo слил их в outbox в той же tx
})
```

Если упасть между двумя вызовами в первом варианте — событие потеряно навсегда.

**3. Cron + SQL-скрипт мимо домена.**

Прямой UPDATE статуса обходит все guard-ы агрегата. Анти-снайп-продление остаётся в базе (ends_at уже обновлён), а статус ставится 'closed'. Две версии правды об одном аукционе.

**4. Отсутствие partial index.**

Без `WHERE status = 'listed'` в индексе скан проходит по всем аукционам всех времён. На OLTP-таблице в 10 миллионов строк это будет seq scan по 10M строк раз в секунду.

**5. Воркер, проверяющий состояние перед командой.**

```go
// Неправильно: двойное чтение создаёт TOCTOU
status, _ := repo.GetStatus(ctx, id)
if status == "listed" {
    closeAuction.Handle(ctx, CloseAuction{id})
}
```

Между `GetStatus` и `Handle` аукцион может быть закрыт конкурентным воркером. Двойное чтение не устраняет гонку — оно создаёт иллюзию безопасности. Guard агрегата под FOR UPDATE — единственное корректное место проверки.

---

## Чек-лист

- [ ] Дедлайн хранится как атрибут агрегата (`ends_at`, `due_at`), а не вычисляется воркером
- [ ] Кандидатный скан — отдельный метод с partial index, без блокировок
- [ ] Команда на каждого кандидата — обычная доменная команда через Use Case
- [ ] Идемпотентность реализована guard-ами агрегата; воркер не проверяет состояние
- [ ] Handler маппит «доброкачественные» sentinel-ы (`ErrAlreadyClosed`, `ErrBiddingStillOpen`) в nil
- [ ] `clock` — consumer-side interface; `time.Now()` не вызывается в доменном слое
- [ ] Воркер запускается в errgroup с HTTP и шиной; `Run` возвращает nil при ctx.Done
- [ ] `molot_worker_tick_duration` и `molot_worker_due_backlog` пишутся каждым тиком
- [ ] Span `worker/closing.tick` охватывает весь батч; дочерние — каждая команда
- [ ] Partial index покрывает предикат скана и протестирован в repository suite
- [ ] Race-тест: Close vs PlaceBid в снайп-окне — оба исхода консистентны
- [ ] Race-тест: 20 конкурентных ставок — ровно один победитель

---

## Ссылки

- ARCHITECTURE §10 — «Закрытие по времени и таймаут оплаты — два воркера в бинаре монолита»
- ARCHITECTURE §2.1 — Guard-таблица агрегата `Auction`: `ErrAlreadyClosed`, `ErrBiddingStillOpen`
- ARCHITECTURE §2.2 — Guard-таблица `Invoice`: `Expire` и nil-сентинели
- ARCHITECTURE §7 — Схема БД: `auctions_due_idx`, `invoices_due_idx`
- ARCHITECTURE §11 — Метрики воркеров: `molot_worker_tick_duration`, `molot_worker_due_backlog`
- `internal/auction/ports/worker.go` — ClosingWorker
- `internal/billing/ports/worker.go` — ExpiryWorker
- `internal/auction/app/command/close_auction.go` — CloseAuctionHandler
- `internal/auction/domain/auction/antisnipe.go` — AntiSnipePolicy
- `internal/auction/adapters/migrations/00001_auction_schema.sql` — `auctions_due_idx`
- `internal/billing/adapters/migrations/00001_invoices.sql` — `invoices_due_idx`
- `internal/auction/adapters/repository_suite_test.go` — race-тесты
- `internal/auction/domain/auction/antisnipe_test.go` — тесты анти-снайпа
- `internal/auction/ports/worker_test.go` — тест оркестрации воркера
