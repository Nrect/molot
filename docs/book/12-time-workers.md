# Глава 12. Время как триггер: закрытие по дедлайну без cron

## Зачем читать

Каждый аукцион должен закрыться ровно тогда, когда истёк его дедлайн. Каждый неоплаченный счёт — истечь ровно через payment term. Задачи звучат так буднично, что рука сама тянется написать «сделаем cron, поставим скрипт». Не спешите. Именно на этом месте в зрелых системах живут самые неприятные баги: потерянные события, гонки между планировщиком и пользователем, инциденты, которые воспроизводятся только в проде и только в три часа ночи.

В этой главе разобран другой подход: **воркер внутри бинара**, написанный как тонкий оркестратор поверх доменных команд. Никакого отдельного cron-демона. Никакого скрипта, который лезет в базу в обход домена. Идемпотентность — в агрегате, а не в воркере. Тесты — детерминированные, без единого sleep. Если коротко: время становится обычным входом домена, а не внешней силой, которая дёргает базу напрямую.

Весь разбор опирается на реальный код проекта Molot; ничего не придумано.

---

## Проблема: почему cron снаружи домена ломается

Классическая схема выглядит так: cron раз в минуту запускает SQL-скрипт, который обновляет строки напрямую, публикует события и на этом заканчивает. Внедряется за вечер, выглядит безобидно — поэтому и встречается повсеместно. Дефектов в этой схеме три, и все три системные: их нельзя починить аккуратностью, только сменой конструкции.

**Дефект 1: транзакция расщеплена между двумя участниками.** Скрипт делает `UPDATE auctions SET status='closed'`, потом отдельным вызовом публикует событие в очередь. Между этими двумя операциями процесс может упасть. Событие потеряно, статус в базе уже изменён — домен и шина рассинхронизированы, и никакой ретрай этого не исправит. Outbox-паттерн решает эту проблему, но только если запись в outbox происходит **в той же транзакции**, что и мутация агрегата. Скрипт снаружи это условие нарушает по самой своей природе.

**Дефект 2: обход доменных инвариантов.** Агрегат знает то, чего скрипт не узнает никогда: если ставка продлила окно, закрытие раньше нового дедлайна невозможно. Скрипт пишет `WHERE ends_at <= now()` и с чистой совестью закрывает то, что закрывать ещё нельзя. Анти-снайп-политика обнуляется — и заметит это не мониторинг, а проигравший участник аукциона.

**Дефект 3: деплой и тестируемость.** Cron-скрипт — отдельный артефакт деплоя: своя версия, своё расписание, своя конфигурация, свой способ разъехаться с версией приложения. Его поведение в тестах не проверяется вместе с доменом. А гонка между cron и пользовательской ставкой тестируется... никогда, потому что для этого нужен реальный планировщик.

---

## Теория: паттерн «лёгкий скан + команда на каждого»

Конструкция, которая работает, собирается из трёх элементов. Каждый по отдельности скучен — вместе они снимают все три дефекта.

**Дедлайн — данное агрегата, не вычисление воркера.** Воркер не должен решать, истёк ли дедлайн: у него для этого нет ни знаний, ни полномочий. Его работа — найти кандидатов, у которых это уже произошло, и попросить домен разобраться. Логика «истёк ли дедлайн» живёт в guard-е агрегата, и именно там, под локом, проверяется `now >= endsAt`.

**Скан без блокировок — только для нахождения кандидатов.** `SELECT id FROM ... WHERE ends_at <= now() AND status = 'listed'` — это не мутация, это подсказка, и относиться к ней нужно как к подсказке. Список кандидатов может устареть в ту же миллисекунду — и это нормально: если аукцион уже закрыт или дедлайн продлён, guard агрегата вернёт соответствующую ошибку, и воркер её спокойно проигнорирует. Блокировку на этапе скана не берём; contention возникает только там, где идёт реальная запись.

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

Смотрите на `WHERE status = 'listed'` / `WHERE status = 'pending'` — это предикат partial index, и в нём вся суть. В индекс попадают только строки в «живом» состоянии; закрытые аукционы и оплаченные счета не входят в него совсем. Таблица может расти годами — скан кандидатов не замедлится, потому что индекс хранит только то немногое, что ещё может «дозреть».

> **Где вы на это наступите.** Partial index работает, только пока планировщик может доказать, что предикат запроса влечёт предикат индекса. Поменяете запрос с `status = 'listed'` на `status IN ('listed', 'suspended')` — и Postgres молча уйдёт в seq scan, без ошибок и предупреждений. После любого изменения кандидатного запроса или предиката индекса смотрите `EXPLAIN` — это единственный способ узнать правду.

**Идемпотентность — в guard агрегата, не в воркере.** Воркер не проверяет, закрыт ли уже аукцион, — он просто вызывает `CloseAuction{AuctionID}` и идёт дальше. Handler вызывает `a.Close(now)`; guard агрегата возвращает `ErrAlreadyClosed`; handler маппит его в `nil`. Ни одного события в outbox не записано — транзакция была откатана самим guard-ом, прежде чем что-то закоммитилось. Повторный вызов абсолютно безопасен. Воркер в этой схеме — курьер, который звонит в дверь: открывать ли и что делать дальше, решает хозяин-агрегат. Курьеру не положено иметь мнение о содержимом посылки.

---

## Как в Molot: ClosingWorker

Теперь к коду. Воркер закрытия аукционов живёт в `internal/auction/ports/worker.go`, и его цикл прост до скучности — в данном случае это комплимент:

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

Самое важное в этом листинге — то, чего здесь **нет**: ни проверки статуса аукциона, ни `if alreadyClosed`, ни ретрая с задержкой, ни логики дедлайна. Воркер не знает о домене ничего, кроме одного факта: кандидата нужно отдать команде `CloseAuction`. Все решения — в handler-е и агрегате, и именно поэтому воркер не придётся трогать, когда изменятся правила закрытия.

> **Нюанс.** `time.Ticker` в Go не копит пропущенные тики: если обработка батча заняла дольше интервала, лишние тики просто выпадают. Для воркера это удачное свойство — после паузы он не бросится «догонять» лавиной из десятков тиков, а спокойно продолжит со следующего. Но помните об этом, оценивая частоту обработки: интервал тикера — нижняя граница, а не гарантия.

Вся политика обработки ошибок умещается в handler — и читается как таблица:

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

Три sentinel-а превращаются в `nil`. На ревью это место регулярно вызывает вопрос «а не глотаем ли мы ошибки?» — нет: это явный контракт, для воркера эти исходы доброкачественны. `ErrBiddingStillOpen` особенно показателен: он означает, что ставка успела продлить окно между сканом и `Close`. Закрывать нечего, воркер подождёт следующего тика — система отработала ровно так, как задумано.

## Как в Molot: ExpiryWorker

Спроектировав схему один раз, второй воркер вы получаете почти бесплатно. Воркер истечения счетов (`internal/billing/ports/worker.go`) устроен зеркально:

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

Разница в одном, и она стоит внимания: `dueInvoices` — consumer-side interface, отдельный от полного `Repository`. Воркер видит ровно один метод — `PendingDueBefore(ctx, t, limit)` — и больше ничего. Это намеренное сужение: невозможно «по-быстрому» дописать в воркер логику поверх репозитория, если у воркера физически нет репозитория.

## Дедлайн как данное агрегата, не как расписание воркера

Принципиальный момент, который отличает этот дизайн от cron-подхода: **воркер не хранит дедлайны**. Дедлайн — атрибут агрегата, такой же, как цена или статус.

Для счёта: `due_at = issued_at + PAYMENT_TERM`. Поле записывается при создании `Invoice` в домене и хранится в строке `billing.invoices`. Воркер лишь спрашивает: «какие pending-счета уже прошли свой due_at?» — и получает список готовых ID.

Для аукциона: `ends_at` хранится в строке агрегата и может быть продлён анти-снайп-политикой. Воркер спрашивает: «какие listed-аукционы имеют ends_at в прошлом?» — и получает кандидатов. Заметьте: продление дедлайна не требует «перепланировать задачу» — следующий скан просто не увидит этот аукцион среди кандидатов.

Сага при этом **не спит и не хранит таймеров**. Она реагирует на события: прилетело `InvoiceExpiredV1` — значит, воркер сделал своё дело. Saga не знает, когда именно истечёт срок, — ей и не нужно: она реагирует на факт истечения, а не на расписание.

---

## Гонка PlaceBid против Close в снайп-окне

Это самая интересная гонка в системе — и лучший экзамен для всей конструкции. Разрешается она без координатора, без распределённых локов и без единой строчки специального кода в воркере.

**Сценарий.** Воркер сканирует кандидатов в момент `T`. Аукцион попадает в список: его `ends_at = T - 1s`. Одновременно пользователь размещает ставку в последнюю секунду, которая попадает в анти-снайп-окно (5 минут до дедлайна) и продлевает `ends_at` на 10 минут. Два UPDATE идут к одной строке.

**Механизм.** Оба пути — `PlaceBid` и `Close` — проходят через `repo.UpdateAsSystem` / `repo.Update`, внутри которых `SELECT ... FOR UPDATE` на строку аукциона. Один из них захватит row lock первым.

*Сценарий A: ставка пришла первой.* `PlaceBid` захватывает lock, продлевает `ends_at`, коммитит. `Close` получает lock, перечитывает агрегат с новым `ends_at` и вызывает `a.Close(now)`. Guard агрегата проверяет: `now < endsAt`, поэтому возвращает `ErrBiddingStillOpen`. Handler маппит в nil. Аукцион не закрыт. Следующий тик подберёт его.

*Сценарий B: Close пришёл первым.* `Close` захватывает lock, закрывает аукцион, коммитит `AuctionClosedV1`. `PlaceBid` получает lock, перечитывает — статус `closed`, значит `ErrAuctionNotOpen`, а это HTTP 409. Опоздавшая ставка отклонена.

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

Присмотритесь к switch-у в конце: тест не знает, кто победит, и не пытается узнать. Он требует другого — чтобы любой из двух исходов был внутренне консистентен, а всё третье считалось провалом. Это и есть корректность без координатора: не «гонки нет», а «любой исход гонки легален».

## Анти-снайп-политика как снапшот агрегата

Последний кирпич конструкции — сама политика анти-снайпа. Её принципиально не читают из конфига при каждой ставке: политика снапшотится на агрегат в момент листинга и хранится в трёх колонках:

```sql
-- internal/auction/adapters/migrations/00001_auction_schema.sql
snipe_window_sec    int         NOT NULL,        -- anti-snipe policy snapshot
snipe_extension_sec int         NOT NULL,
snipe_max_ext       int         NOT NULL,
```

Три колонки вместо ссылки на конфиг — это не денормализация по небрежности, а гарантия: правила лота зафиксированы в момент листинга. Доменный объект:

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

`TriggersAt` — чистая функция от двух моментов времени: ни конфига, ни глобальных часов, ни скрытого состояния. Тест проверяет граничный случай — именно то место, где наивная реализация ошибается чаще всего:

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

Прочитайте комментарий внутри теста: воркер просканировал до продления — закрытие по старому дедлайну обязано быть отвергнуто. Это сценарий A из гонки выше, сведённый к чистому домену и прогоняемый за наносекунды.

А снапшот политики гарантирует ещё одно: смена платформенных правил анти-снайпа не влияет на уже идущие аукционы. Правила лота — как правила настольной партии: их фиксируют до первого хода, а не меняют посреди игры. Лот детерминирован во времени.

---

## Несколько реплик: почему гонка доброкачественна

Запустите несколько реплик монолита — и оба воркера закрытия рано или поздно получат одинаковый список кандидатов из скана (скан без блокировок, это штатно). Оба попытаются закрыть один и тот же аукцион. Звучит как проблема, но вопрос «кто кого» здесь даже не возникает.

Первый захватывает row lock, закрывает, коммитит. Второй захватывает lock, перечитывает агрегат — `status = 'closed'`, значит `ErrAlreadyClosed`, который маппится в nil. Никаких дублирующих событий: outbox пишется только внутри той транзакции, которая реально изменила статус. Проигравшая транзакция была откатана целиком — для системы её как будто и не было.

Дополнительно работает optimistic lock: `UPDATE ... WHERE id = $1 AND version = $expected`. Это belt-and-suspenders поверх FOR UPDATE — если вдруг две транзакции прошли мимо блокировки (что теоретически невозможно при корректном FOR UPDATE, но страхует от ошибок реализации), UPDATE одной из них затронет 0 строк и попадёт в ветку ошибки.

Если contention реплик станет измеримым (сигнал — метрика `molot_worker_lock_wait`), правильная мера — случайная фаза тикера на реплику через `CLOSING_POLL_JITTER`: сканы разъезжаются во времени, и коллизии редеют. А вот `SKIP LOCKED` в кандидатном скане неприменим, как бы ни хотелось его сюда вписать: скан безлоковый по замыслу, и корректность держат guard-ы агрегата, а не блокировки в скане.

---

## Запуск в errgroup: воркер — полноправный горутин монолита

Остался вопрос деплоя — и ответ на него короткий: отдельного деплоя у воркера нет. Оба воркера запускаются из одного `errgroup` вместе с HTTP-сервером и Watermill-роутером:

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

Ключевая деталь: `Run` воркера возвращает `nil` при отмене контекста — shutdown чистый и не отравляет errgroup ложной ошибкой. Цепочка остановки прозрачна: `SIGTERM` приходит в `signal.NotifyContext`, отменяет верхний ctx, errgroup разносит отмену, все горутины останавливаются в порядке отмены. Воркер умирает вместе с приложением и рождается вместе с ним — рассинхрон версий между «кодом» и «планировщиком» невозможен по построению.

---

## Наблюдаемость воркеров

Фоновый процесс, о котором никто ничего не знает, — мина замедленного действия: он может молча не работать неделями, и обнаружится это по жалобам пользователей. Поэтому каждый тик воркера оборачивается span-ом. Из кода `ClosingWorker.tick`:

```go
ctx, span := w.tracer.Start(ctx, "worker/closing.tick")
defer span.End()
```

Из этого span-а дочерними становятся `commands/CloseAuction` — один на каждый аукцион в батче. Итог: в трейсе видно, сколько аукционов закрыл один тик, какой из них занял больше всего времени.

Метрики:
- `molot_worker_tick_duration{worker="closing"}` — гистограмма длительности тика. Рост p99 означает, что батч вырос или БД перегружена.
- `molot_worker_due_backlog{worker="closing"}` — количество кандидатов, найденных в последнем тике. Постоянный рост — воркер не успевает обработать батч за интервал тикера.

Алерт по `backlog > threshold` — это и есть сигнал, что интервал тикера нужно уменьшить или параллелизм увеличить.

> **Совет из практики.** Из двух метрик именно backlog — опережающий индикатор. Длительность тика расскажет, что воркеру тяжело; растущий backlog — что он уже не справляется и дедлайны лежат необработанными. Стройте алерт на тренде backlog за несколько тиков, а не на разовом значении: единичный всплеск после рестарта — норма, монотонный рост — инцидент.

---

## Тестируемость: часы как параметр

Всё описанное было бы трудно доказать тестами, будь время глобальным. Поэтому воркер принимает `clock interface{ Now() time.Time }`, а агрегаты — `now time.Time` явным параметром. Ни одного вызова `time.Now()` в доменном или app-слое: время — обычный вход, который тест задаёт сам.

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

Заметьте границу: тест не спрашивает «правильно ли закрылся аукцион» — это вопрос к домену и его guard-ам. Он спрашивает ровно одно: «отправил ли воркер CloseAuction с правильным ID». Stub-сканер, stub-часы, записывающий handler — разделение ответственности в действии.

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

`end.Add(-3*time.Minute)` — просто `time.Time`, переданный в `PlaceBid`: ни таймеров, ни ожиданий. Такой тест запускается за наносекунды и не флакает никогда — ему попросту нечем флакать.

---

## Трейдоффы

Сводка решений и их цены — без неё разговор с командой о «почему не cron» рискует превратиться в спор вкусов:

| Решение | Плюс | Минус |
|---|---|---|
| Воркер в бинаре | Outbox атомарен с мутацией; тест с реальным доменом; один деплой | Если сервис упал — воркер тоже упал; нужна горизонтальная устойчивость |
| Partial index | O(кандидаты) без роста с таблицей | Нужна дисциплина: при добавлении нового состояния — проверить, не должен ли индекс его включить |
| Идемпотентность в агрегате | Воркер «тупой», любой вызов безопасен | Guard-ы нужно тщательно проектировать и покрывать тестами — случайный no-op опасен |
| Несколько реплик | Нет единой точки отказа | Гонки реплик потребляют row lock; при высоком TPS может потребоваться jitter |
| `ErrBiddingStillOpen` → nil | Опоздавший Close — не ошибка, нормальное течение | Нельзя использовать этот sentinel для детектирования реальных проблем |

---

## Типичные ошибки

Пять граблей, на которые наступают почти все воркеры по расписанию. Проверьте свои.

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

Sleep-цикл не умеет просыпаться по отмене контекста: процесс уже получил SIGTERM, а воркер ещё пять секунд спит и, чего доброго, начинает новый батч. Shutdown превращается в гонку.

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

Если упасть между двумя вызовами в первом варианте — событие потеряно навсегда, и никакой ретрай его не вернёт: статус-то уже закоммичен.

**3. Cron + SQL-скрипт мимо домена.**

Прямой UPDATE статуса обходит все guard-ы агрегата. Анти-снайп-продление остаётся в базе (ends_at уже обновлён), а статус ставится 'closed'. Две версии правды об одном аукционе — и разбираться, какая настоящая, придётся вам.

**4. Отсутствие partial index.**

Без `WHERE status = 'listed'` в индексе скан проходит по всем аукционам всех времён. На OLTP-таблице в 10 миллионов строк это будет seq scan по 10M строк раз в секунду — база заметит, и вы тоже.

**5. Воркер, проверяющий состояние перед командой.**

```go
// Неправильно: двойное чтение создаёт TOCTOU
status, _ := repo.GetStatus(ctx, id)
if status == "listed" {
    closeAuction.Handle(ctx, CloseAuction{id})
}
```

Между `GetStatus` и `Handle` аукцион может быть закрыт конкурентным воркером. Двойное чтение не устраняет гонку — оно лишь дарит автору ощущение, что он что-то проверил. Guard агрегата под FOR UPDATE — единственное корректное место проверки.

---

## Чек-лист

Прежде чем считать воркер по расписанию готовым:

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
