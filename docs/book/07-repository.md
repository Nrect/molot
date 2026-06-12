# 07. Repository: updateFn, транзакции, конкуренция

## Зачем эта глава

Repository — самый «затасканный» паттерн в Go-проектах и одновременно самый часто изуродованный. В девяти проектах из десяти под этим словом живёт нечто среднее между DAO с сорока методами и сервисным слоем, в который протекла половина бизнес-логики. Если узнали свой проект — не расстраивайтесь: вы в большинстве. А ведь у паттерна одна работа: **превратить бизнес-операцию «прочитай-измени-сохрани» в атомарную и конкурентно-безопасную — так, чтобы ни домен, ни вызывающий код не знали, как именно это достигнуто**.

В этой главе разберём, как репозиторий устроен в Molot: минимальный интерфейс в домене, мутации через `updateFn`-замыкание, `SELECT ... FOR UPDATE` плюс optimistic version, идиома `FinishTransaction`, маппинг через `UnmarshalFromDatabase` и in-memory реализация, которая гоняется тем же тест-сьютом, что и Postgres. Каждое решение — с ответом «почему так, а не иначе».

## Проблема

Возьмём центральную операцию аукциона — ставку. Наивный код выглядит так:

```go
// АНТИПРИМЕР — так в Molot не делается
a, _ := repo.Get(ctx, id)        // 1. прочитали
a.PlaceBid(...)                  // 2. изменили в памяти
repo.Save(ctx, a)                // 3. сохранили
```

Три строки, читается прекрасно — и в них зарыто сразу четыре мины:

1. **Lost update.** Две горутины читают одну версию агрегата, обе бьют ставку, обе сохраняют — вторая молча затирает первую. На дев-стенде не воспроизводится никогда; в проде проявляется на самом горячем лоте, в последнюю минуту торгов. У таких багов отменное чувство момента.
2. **Нет атомарности.** Между Get и Save процесс может упасть, а если Save пишет в несколько таблиц (snapshot + история ставок + outbox) — упасть может между ними.
3. **Транзакция течёт вверх.** Чтобы закрыть пункты 1–2, тянутся к `*sql.Tx` в сервисе — и вот уже бизнес-слой импортирует `database/sql`, а смена хранилища означает переписывание use case-ов.
4. **Размазанные guard-ы.** Кто проверяет права на мутацию? Если вызывающий — однажды забудут (см. главу о security: пропущенный `if` в Harbor стал CVE).

Repository в Molot закрывает все четыре пункта одним контрактом. Чтобы понять, каким, — немного теории.

## Теория

### Интерфейс — в домене, реализация — в адаптере

Первый вопрос: где живёт сам интерфейс? Его объявляет **владелец инварианта**, то есть доменный пакет агрегата (BOOK_AUDIT, правило 15). Не «рядом с реализацией» — это перевернуло бы стрелку зависимости: домен начал бы знать про адаптеры, появились бы циклы импортов, а интерфейс пух бы под нужды конкретной БД. Когда интерфейс живёт в `domain/`, реализация *подстраивается под домен*, а не наоборот — и их может быть сколько угодно (Postgres, in-memory, завтра — что-то ещё), все взаимозаменяемы by construction.

Второе правило — **минимальность**: `Add / Get / Update`, и всё. Per-use-case методы (`ApproveReschedule(...)`, `PlaceBidOnAuction(...)`) — антипаттерн: репозиторий впитывает семантику приложения, и каждый новый use case требует править интерфейс, обе реализации и общий тест-сьют. Семантика операции должна жить в *замыкании*, которое передаётся в универсальный `Update`.

### updateFn: контракт мутации

Каноническая сигнатура:

```go
Update(ctx, id, actor, updateFn func(ctx, *X) (*X, error)) error
```

Контракт адаптера — строго фиксированная последовательность **внутри одной транзакции**:

1. **load** — прочитать строку с блокировкой, размаппить в доменный тип;
2. **guard** — проверки доступа (если применимы) на свежепрочитанном агрегате;
3. **fn** — вызвать `updateFn`; ошибка из замыкания = rollback всей транзакции;
4. **persist возвращённого** — сохранить именно то значение, которое замыкание вернуло, инкрементировав версию.

Почему `updateFn` **возвращает** агрегат, а не просто мутирует переданный указатель? Три причины:

- **Явный коммит намерения.** `return a, nil` читается как «вот это сохрани»; `return nil, err` — «ничего не сохраняй». Мутация указателя такой однозначности не даёт: адаптеру пришлось бы гадать, надо ли сохранять после ошибки наполовину изменённый объект.
- **Гибкость подменить экземпляр.** Замыкание вправе вернуть другой объект (пересобранный, обогащённый) — контракт это допускает без изменения сигнатуры.
- **Тестируемость rollback-а.** Тест может нарочно мутировать агрегат и вернуть ошибку — и проверить, что в хранилище не протекло ничего (увидим ниже в shared suite).

И почему **транзакции через `context` запрещены** (BOOK_AUDIT, правило 16)? Потому что это магия: по сигнатуре функции невозможно понять, выполняется она в транзакции или нет; middleware, открывающий транзакцию «на весь запрос», держит блокировки на время сетевых вызовов; а тот, кто достаёт `tx` из контекста, молча связывает себя с конкретным драйвером. Транзакция — собственность адаптера, и её границы должны совпадать с границами одного вызова `Update`. Как кухня в ресторане: гость описывает блюдо (замыкание говорит, *что* сделать), но к плите его не пускают — огнём владеет повар. Точка.

### FOR UPDATE + optimistic version: зачем оба

`SELECT ... FOR UPDATE` сериализует конкурентов: вторая транзакция засыпает на локе и, проснувшись, видит уже изменённую строку. Этого достаточно... ровно до тех пор, пока **каждый** пишущий код-путь проходит через лок. А в реальной жизни появляются миграции данных, ops-правки руками, новый код-путь, написанный через год человеком, который не знал про конвенцию. Optimistic version (`WHERE id = $1 AND version = $17`) — вторая линия: запись, сделанная в обход лока, превратит тихий lost update в громкую ошибку «optimistic lock conflict». Цена — одно целочисленное сравнение в WHERE. Это belt and suspenders: ремень держит штаны каждый день, подтяжки — в тот единственный день, когда ремень порвался.

> **Где вы на это наступите.** Сценарий, ради которого существует вторая линия, выглядит буднично: год спустя кто-то пишет скрипт массовой правки или новый воркер, читает строку обычным SELECT — без FOR UPDATE, потому что про конвенцию не знал, — и пишет обратно. Без version-проверки это тихий lost update, который всплывёт через неделю жалобой пользователя «у меня была другая ставка». С version-проверкой — громкая ошибка в логах в ту же секунду. Из двух плохих новостей выбирайте ту, что приходит сразу.

### Ошибки: драйвер не протекает выше repo

`sql.ErrNoRows`, `pgconn.PgError` и прочие артефакты драйвера — словарь адаптера. Выше repo они не выходят: not-found маппится в доменную `NotFoundError{ID}`, нарушение уникального констрейнта — в доменный sentinel, всё остальное оборачивается с человеческим контекстом. Иначе app-слой начинает импортировать драйвер ради `errors.Is(err, sql.ErrNoRows)` — и независимость слоёв закончилась, не успев начаться.

### In-memory реализация — не игрушка

Каждому интерфейсу repo полагается in-memory реализация (BOOK_AUDIT, правило 20). Не для «моков» — для **domain-first разработки**: новый агрегат пишется и тестируется против карты в памяти, пока решение о хранилище отложено. И для скорости: app-тесты и unit-уровень shared suite бегают без Docker. Ключевая тонкость — карта хранит **значения, а не указатели**: иначе вызывающий, удержавший `*Auction` после `Get`, мутировал бы хранилище в обход `Update`, и репозиторий перестал бы быть единственной дверью к состоянию.

А чтобы две реализации не разъехались семантически, на обе натравливается **один и тот же тест-сьют** — две реализации сдают один экзаменационный билет, и списать друг у друга не выйдет. Suite — это исполняемый контракт поведения интерфейса: идемпотентность Add, rollback при ошибке замыкания, инкремент версии, гонки. Postgres-реализация проверяет «правильно ли мы используем БД», in-memory — «правильно ли мы поняли собственный контракт».

Теория на месте — смотрим код.

## Как в Molot

### Интерфейс

`internal/auction/domain/auction/repository.go` — целиком, он того стоит:

```go
// Repository is the minimal aggregate store (rules 15-16, 21, 23).
// Mutations go through update closures: the adapter owns the
// transaction (SELECT ... FOR UPDATE + version), persists the returned
// aggregate, appends bids from recorded BidPlaced events to the
// append-only history and publishes mapped integration events through
// the outbox — all in one transaction.
type Repository interface {
	// Add persists a new aggregate. Idempotent: an id conflict is a
	// silent no-op (ON CONFLICT (id) DO NOTHING — no events re-emitted);
	// a relist_of uniqueness conflict returns ErrAlreadyRelisted.
	Add(ctx context.Context, a *Auction) error

	// Get loads an aggregate. The auction card is public, so no actor
	// is required (documented decision, §2.1).
	Get(ctx context.Context, id AuctionID) (*Auction, error)

	// Update runs a user-driven mutation; the acting user is an
	// explicit typed parameter (rule 21), never context values.
	Update(
		ctx context.Context,
		id AuctionID,
		actor Actor,
		updateFn func(ctx context.Context, a *Auction) (*Auction, error),
	) error

	// UpdateAsSystem is Update for system flows — the closing worker
	// and the settlement saga. The name shouts about the security
	// implication (rule 23).
	UpdateAsSystem(
		ctx context.Context,
		id AuctionID,
		updateFn func(ctx context.Context, a *Auction) (*Auction, error),
	) error
}
```

Четыре метода, и четвёртый — не нарушение минимальности, а security-решение: системные потоки (воркер закрытия, сага расчётов) не маскируются под фейковых пользователей, у них отдельный метод с именем, кричащим об импликации. Заметьте: `actor` — обязательный типизированный параметр `Update`, его невозможно «забыть», код без него не скомпилируется.

Вызывающая сторона — app-слой, `internal/auction/app/command/place_bid.go`:

```go
err = h.repo.Update(ctx, cmd.AuctionID, auction.ActorFromBidder(cmd.Bidder),
	func(ctx context.Context, a *auction.Auction) (*auction.Auction, error) {
		if _, err := a.PlaceBid(cmd.BidID, bidder, cmd.Amount, h.clock.Now()); err != nil {
			return nil, err
		}
		return a, nil
	})
```

Хендлер не знает ни про транзакции, ни про локи, ни про SQL. Вся бизнес-семантика — один вызов behavior-метода домена внутри замыкания. Это и есть разделение труда: домен решает «можно ли», адаптер гарантирует «атомарно и без гонок».

### Postgres: load → fn → persist под двойной защитой

Теперь сторона адаптера. Ядро — приватный `update`, `internal/auction/adapters/auction_pg_repository.go`:

```go
func (r *AuctionPostgresRepository) update(
	ctx context.Context,
	id auction.AuctionID,
	updateFn func(ctx context.Context, a *auction.Auction) (*auction.Auction, error),
) error {
	return postgres.RunInTx(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT `+auctionColumns+` FROM auction.auctions WHERE id = $1 FOR UPDATE`, id.UUID())
		p, err := scanPgAuction(row)
		if errors.Is(err, sql.ErrNoRows) {
			return auction.NotFoundError{AuctionID: id}
		}
		if err != nil {
			return fmt.Errorf("unable to lock auction row: %w", err)
		}
		a, err := toDomainAuction(p)
		if err != nil {
			return err
		}

		updated, err := updateFn(ctx, a)
		if err != nil {
			return err
		}

		up := toPgAuction(updated)
		res, err := tx.ExecContext(ctx, `
			UPDATE auction.auctions SET
				ends_at = $2, extensions_used = $3, status = $4, outcome = $5,
				...
				version = $17 + 1, updated_at = now()
			WHERE id = $1 AND version = $17`,
			...
			p.Version,
		)
```

(многоточия — пропущенные списки колонок; полный текст в файле). Сверьте с контрактом из теории — все четыре фазы на месте: `FOR UPDATE` на load, доменная `NotFoundError` вместо `sql.ErrNoRows`, ошибка замыкания просто возвращается — rollback сделает обвязка, persist пишет **`updated`** (возвращённое значение), и `WHERE ... AND version = $17` с `version = $17 + 1` — это optimistic-проверка поверх уже взятого лока. Что происходит, если она не сошлась:

```go
		if affected == 0 {
			// Belt-and-suspenders to FOR UPDATE: cannot happen unless
			// the row was mutated outside the lock.
			return fmt.Errorf("optimistic lock conflict on auction %s (version %d)", id, p.Version)
		}
```

Комментарий честен: при живом `FOR UPDATE` эта ветка недостижима. Она существует для того дня, когда кто-то изменит строку в обход лока.

Дальше в той же транзакции — ещё две записи: append-only история ставок (из записанных доменных событий `BidPlaced`) и **outbox**:

```go
		events := updated.PullDomainEvents()
		for _, e := range events {
			placed, ok := e.(auction.BidPlaced)
			if !ok {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO auction.auction_bids (bid_id, auction_id, bidder_id, amount_minor, currency, placed_at)
				VALUES ($1,$2,$3,$4,$5,$6)`,
				...
			); err != nil {
				return fmt.Errorf("unable to append bid to history: %w", err)
			}
		}
		return r.publishMapped(ctx, tx, updated, events)
	})
}
```

Ключевая строка — последняя: `publishMapped` привязывает watermill-publisher к **той же `tx`**, и интеграционное событие коммитится или откатывается вместе с бизнес-записью. Это transactional outbox; почему двойная запись «БД + брокер» без него теряет события — разобрано в главе о событиях, здесь важно одно: outbox — не отдельный механизм «рядом» с репозиторием, а ещё одна запись внутри того же `update`. Интеграционный тест `TestPostgresOutbox` (`internal/auction/adapters/pg_integration_test.go`) прямо ассертит: после успешного Update в outbox-таблице +1 строка, после rollback-а — ноль новых.

### finishTransaction: не потерять оригинальную ошибку

Всё это держится на обвязке `RunInTx` — её стоит прочитать построчно, `internal/common/postgres/postgres.go`:

```go
// RunInTx executes fn inside a transaction: commit when fn returns nil,
// rollback otherwise. The deferred FinishTransaction sees the named
// return error, so fn can simply return — no manual commit/rollback in
// call sites.
func RunInTx(ctx context.Context, db *sql.DB, fn func(ctx context.Context, tx *sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	defer func() {
		err = FinishTransaction(err, tx)
	}()

	return fn(ctx, tx)
}

// FinishTransaction rolls back tx when err is non-nil (combining a
// failed rollback with the original error so neither is lost) and
// commits otherwise.
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

Здесь три неочевидных решения, каждое — против конкретного бага:

- **Named return `(err error)`.** Без него deferred-функция не видела бы итоговую ошибку и не могла бы её подменить. Это единственный способ в Go сделать «коммить, если успех; откатывай, если нет» одним defer-ом — и убрать ручные `tx.Commit()/tx.Rollback()` из всех call site-ов (где их регулярно забывают на ранних `return`).
- **`multierr.Combine` при упавшем rollback.** Наивный `return rollbackErr` — классическая потеря первопричины: в логах останется «rollback tx: connection reset», а *почему* транзакция откатывалась (та самая бизнес-ошибка) исчезнет навсегда. Combine сохраняет обе.
- **Идиома экспортирована.** Адаптер, которому нужен собственный `BeginTx`, обязан финишировать так же: `defer func() { err = postgres.FinishTransaction(err, tx) }()` — одна реализация на кодбейс, ноль самодеятельности.

> **Нюанс.** Идиома с named return хрупче, чем кажется. Достаточно будущему редактору `RunInTx` «причесать» сигнатуру до безымянной `error` или вернуть результат через локальную переменную — и deferred-функция перестанет влиять на итоговую ошибку: коммиты начнут проходить там, где должен был быть rollback. Компилятор промолчит, happy-path-тесты промолчат. Если трогаете эту функцию — перечитайте её тесты до правки, а не после.

### Гонки: тесты, которые держат всё это вместе

Слова «конкурентно-безопасно» без тестов — лирика. Shared suite (`internal/auction/adapters/repository_suite_test.go`) содержит два race-теста; оба бегают и против in-memory (unit-уровень), и против реального Postgres (под build tag `integration`), всегда с `-race`.

**Тест первый: 20 горутин, одна сумма, ровно один победитель.**

```go
	t.Run("race: 20 concurrent equal bids, exactly one winner", func(t *testing.T) {
		...
		const bidders = 20
		start := make(chan struct{})
		winners := make(chan auction.BidID, bidders)
		...
		for i := 0; i < bidders; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				bidder := verifiedBidderFx(t)
				bidID := newBidIDFx(t)
				<-start
				err := repo.Update(ctx, a.ID(), auction.ActorFromBidder(bidder.ID()),
					func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
						if _, err := current.PlaceBid(bidID, bidder, eurFx(t, 1000), t0.Add(time.Hour)); err != nil {
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
```

Все 20 горутин освобождаются одним `close(start)` и бьют **одинаковую** сумму. Механика: первая взявшая лок проходит, остальные 19 просыпаются на локе, перечитывают агрегат, и доменный guard `ErrBidBelowMinimum` их честно отбрасывает — ставка 1000 уже не превышает `MinimalNextBid`. Ассерты дальше по тесту: победитель ровно один, `BidCount == 1`, `Version == 2` (ровно один успешный Update). Заметьте, что именно проверяется: не «лок взялся» (это тест мока), а наблюдаемый бизнес-исход.

**Тест второй: ставка против закрытия в снайп-окне.** Самая красивая из закрытых гонок. Воркер закрытия нашёл лот кандидатным сканом (`DueForClosing` — без локов), но между сканом и `Close` успевает прилететь снайп-ставка, продлевающая окно:

```go
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
			...
		default:
			t.Fatalf("no consistent winner: closeErr=%v bidErr=%v", closeErr, bidErr)
		}
```

Смотрите, как сформулирован тест: он не навязывает исход, а перечисляет два допустимых мира — и объявляет любой третий багом. Оба пути сериализуются локом на одной строке, и оба после захвата лока работают со **свежеперечитанным** агрегатом — это даёт контракт updateFn бесплатно. Если первой прошла ставка, `Close` внутри своей транзакции видит уже продлённое окно, и доменный guard отвечает `ErrBiddingStillOpen` (`internal/auction/domain/auction/auction.go`):

```go
// Close hammers the auction once the (possibly extended) window has
// elapsed. ErrBiddingStillOpen is critical for the worker race: a bid
// may have extended the window after the candidate scan.
func (a *Auction) Close(now time.Time) (ClosingResult, error) {
	...
	if now.Before(a.window.EndsAt()) {
		return ClosingResult{}, ErrBiddingStillOpen
	}
```

Воркер трактует это как «отступи до следующего тика». Если первым прошло закрытие — ставка получает `ErrAuctionNotOpen`. Третьего исхода не существует, и тест падает на любом другом сочетании ошибок. Обратите внимание: гонку разрешает не инфраструктура, а **доменный guard на перечитанном состоянии** — инфраструктура лишь гарантирует, что состояние свежее.

И обязательный rollback-тест — тот самый, ради которого updateFn возвращает значение:

```go
	t.Run("update rolls back when the closure fails", func(t *testing.T) {
		...
		err := repo.Update(ctx, a.ID(), auction.ActorFromBidder(newBidderIDFx(t)),
			func(ctx context.Context, current *auction.Auction) (*auction.Auction, error) {
				// Mutate first, then fail: nothing may leak out.
				if _, err := current.PlaceBid(newBidIDFx(t), verifiedBidderFx(t), eurFx(t, 1000), t0.Add(time.Hour)); err != nil {
					return nil, err
				}
				return nil, sentinel
			})
```

Замыкание сначала *успешно* мутирует агрегат, потом возвращает ошибку — нарочно, в худшем для адаптера порядке. Дальше тест перечитывает: `BidCount == 0`, `Version == 1` — не протекло ничего, ни в snapshot, ни (проверяется отдельно в `TestPostgresOutbox`) в outbox.

> **Совет из практики.** Заводя новую реализацию репозитория — или новый агрегат на старой, — начинайте с прогона shared suite, а не с ручного «вроде работает». И держите `-race` включённым в CI постоянно, а не «для специальных прогонов»: гонка, которую детектор поймал на in-memory реализации за миллисекунды, в проде стоила бы ночного дебага с репродукцией один раз на тысячу запросов.

### Маппинг: storage-структура и UnmarshalFromDatabase

Атомарность и гонки закрыты; остался последний шов — как байты из БД становятся валидным доменным объектом. Домен не знает про БД — значит, у адаптера своя транспортная структура (`internal/auction/adapters/auction_pg_repository.go`):

```go
// pgAuction is the storage model — never shared with domain or
// transport (rule 7); the domain is rebuilt only through
// auction.UnmarshalFromDatabase.
type pgAuction struct {
	ID                  uuid.UUID
	SellerID            uuid.UUID
	Title               string
	...
	ReservePriceMinor   sql.NullInt64
	Outcome             sql.NullString
	LeadingBidID        uuid.NullUUID
	...
	Version             int64
}
```

Здесь видна вся «грязь» хранения, которой нет места в домене: `sql.NullInt64` для отсутствующего резерва, `uuid.NullUUID` для несуществующей ставки, минорные единицы вместо `Money`. (В этом кодбейсе сканирование позиционное через `row.Scan`, поэтому db-теги не нужны; с sqlx та же структура несла бы теги `db:"..."` — суть правила не в тегах, а в том, что структура **приватна и не шарится** со слоями.)

Обратный путь — только через доменную фабрику:

```go
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
```

Почему нельзя собрать `auction.Auction{...}` литералом? Во-первых, физически нельзя — поля unexported, и это не вредность, а та самая гарантия «невалидное состояние непредставимо» из главы о богатой модели. Во-вторых, каждый аргумент фабрики уже прошёл через валидирующий конструктор VO (`NewMoney`, `NewStatusFromString`, `UnmarshalBiddingWindow`...): битая строка в БД — после ручной правки, кривой миграции — превратится в явную ошибку «invalid status in db» на загрузке, а не в паническое поведение домена тремя вызовами позже. Repository — граница доверия: данные из БД не более доверенные, чем данные из HTTP-запроса.

Ошибки маппятся на той же границе (метод `Get` там же):

```go
	p, err := scanPgAuction(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, auction.NotFoundError{AuctionID: id}
	}
	if err != nil {
		return nil, fmt.Errorf("unable to get auction from db: %w", err)
	}
```

Выше repo не выходит ни `sql.ErrNoRows`, ни `pgconn.PgError` — только доменная `NotFoundError` (по которой app-слой строит 404) либо обёрнутая инфраструктурная ошибка с контекстом. Единственное место, где адаптер смотрит на драйверный код ошибки, — маппинг unique violation `auctions_relist_of_uq` в доменный `ErrAlreadyRelisted` внутри `Add`, и наружу опять выходит доменный sentinel.

### In-memory: те же правила, сто строк кода

Вторая реализация того же интерфейса — `internal/auction/adapters/auction_inmem_repository.go`:

```go
// AuctionInMemRepository is the in-memory Repository (rule 20): a map
// of values guarded by an RWMutex; reads return the address of a copy.
type AuctionInMemRepository struct {
	mu       sync.RWMutex
	auctions map[auction.AuctionID]auction.Auction
}
```

Карта **значений** — это не вкусовщина, а несущая стена: `Get` возвращает адрес копии (`cp := stored; return &cp`), и удержанный указатель не даёт мутировать хранилище в обход `Update`. Сам update зеркалит транзакционную семантику Postgres:

```go
func (r *AuctionInMemRepository) update(
	ctx context.Context,
	id auction.AuctionID,
	updateFn func(ctx context.Context, a *auction.Auction) (*auction.Auction, error),
) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	stored, ok := r.auctions[id]
	if !ok {
		return auction.NotFoundError{AuctionID: id}
	}
	working := stored // the closure mutates a copy; failure leaves the map untouched

	updated, err := updateFn(ctx, &working)
	if err != nil {
		return err
	}
	updated.PullDomainEvents()

	bumped, err := cloneWithVersion(updated, updated.Version()+1)
	if err != nil {
		return err
	}
	r.auctions[id] = *bumped
	return nil
}
```

Соответствие 1:1: write-lock на весь update = row lock; `working := stored` (копия!) + запись в карту только при успехе = rollback; `cloneWithVersion` = `version + 1` из SQL — причём версия инкрементируется *через доменную фабрику* `UnmarshalFromDatabase`, потому что сеттера версии у домена нет, она принадлежит адаптерам. Даже идемпотентность `Add` и `ErrAlreadyRelisted` воспроизведены — линейным поиском по карте вместо `UNIQUE(relist_of)`, что для тестового хранилища честная цена.

А сторожит эквивалентность один сьют: `runRepositorySuite(t, newRepo)` вызывается из `inmem_repository_test.go` (unit, без Docker) и из `pg_integration_test.go` (под `//go:build integration`, реальный Postgres) — включая оба race-теста против настоящих row lock-ов. Расхождение семантики реализаций — это красный тест, а не сюрприз в проде.

## Трейдоффы

Честно о том, что мы заплатили:

- **Пессимистичный лок сериализует писателей на строке.** Все ставки одного лота выстраиваются в очередь. Для аукциона это *фича* (порядок ставок и есть бизнес), но throughput на один агрегат ограничен. Сознательное решение здесь — узкая строка-снапшот с денормализованным топ-2 ставок и история ставок отдельной append-only таблицей (ADR-0003): лок держится коротко, история не перечитывается. Если бы писатели не конфликтовали семантически, FOR UPDATE был бы лишним — но у нас конфликтуют.
- **Маппинг — это код.** `toPgAuction`/`toDomainAuction` — ~150 строк скучных присваиваний, и каждое новое поле агрегата трогает структуру, два маппера и `UnmarshalFromDatabase`. Альтернатива — общий struct с тегами на все слои — экономит эти строки и оплачивается утечками формата хранения в API (см. главу о слоях: инцидент `LastIp`). Скучный код дешевле инцидента.
- **updateFn перечитывает агрегат на каждую мутацию.** Никакого кэша между вызовами; зато нет и проблемы инвалидации, а свежесть состояния под локом — то, на чём держится гонка ставка-vs-закрытие.
- **Минимальный интерфейс означает, что сложные выборки идут мимо repo.** `DueForClosing` (кандидатный скан воркера) живёт методом конкретной Postgres-реализации, а каталог и дашборд — вообще отдельные read models (глава о CQRS). Repository обслуживает агрегат, не «все запросы к таблице».
- **In-memory реализация — это вторая реализация.** Её надо поддерживать. Плата окупается скоростью domain-first итераций и тем, что shared suite заставляет проговорить контракт интерфейса явно — иначе «контрактом» молча становится поведение Postgres.

## Типичные ошибки

1. **Per-use-case методы на интерфейсе.** `repo.ApproveReschedule(...)`, `repo.CloseAuction(...)` — репозиторий превращается в сервисный слой, интерфейс растёт с каждым use case, in-memory реализация и suite — тоже. Семантика — в замыкание, интерфейс — `Add/Get/Update`.
2. **`tx` в контексте или поле сервиса.** Невидимые границы транзакций, локи через сетевые вызовы, сцепка слоёв с драйвером. Транзакция живёт и умирает внутри одного вызова адаптера.
3. **Сохранение мутированного аргумента вместо возвращённого значения.** Адаптер, игнорирующий первое возвращаемое значение updateFn, ломает контракт молча — обнаружится, когда замыкание впервые вернёт другой экземпляр.
4. **`defer tx.Rollback()` + ручной `Commit` в конце.** Работает, пока кто-то не вставит ранний `return` после частичной записи. Идиома named err + `FinishTransaction` устойчива к ранним return-ам by construction; и не теряйте оригинальную ошибку при упавшем rollback-е — `multierr.Combine`.
5. **Только optimistic, без лока, при семантическом конфликте писателей.** 19 из 20 ставок получат конфликт версий и уйдут в retry-шторм. Optimistic locking хорош при *редких* конфликтах; горячий агрегат хочет честный лок. И наоборот: только FOR UPDATE без version — тихий lost update от любого кода в обход лока.
6. **`map[K]*V` в in-memory реализации.** Указатель, добытый через Get, мутирует хранилище мимо Update — тесты зеленеют, контракт мёртв. Значения в карте, адрес копии наружу.
7. **`sql.ErrNoRows` в app-слое.** Если для 404 хендлеру нужен импорт `database/sql` — граница продырявлена. Доменная `NotFoundError`, маппинг в адаптере.
8. **Race-тест, проверяющий механику вместо исхода.** «Лок взялся, метод вызвался» — тест мока. Проверяйте бизнес-исход: ровно один победитель, version == 2, состояние консистентно при любом порядке.

## Чек-лист

- [ ] Интерфейс repo объявлен в доменном пакете агрегата; реализаций ≥ 2 (боевая + in-memory).
- [ ] Интерфейс минимален: `Add/Get/Update` (+ явные системные варианты типа `UpdateAsSystem`); per-use-case методов нет; `ctx` — первый параметр.
- [ ] Мутации — только через `updateFn`; адаптер persist-ит **возвращённое** значение; ошибка замыкания = rollback всего (включая outbox).
- [ ] Транзакция не покидает адаптер: ни в context, ни в сигнатурах app-слоя.
- [ ] Read-modify-write — `SELECT ... FOR UPDATE`; поверх — `WHERE ... AND version = $N` с инкрементом; конфликт версий — громкая ошибка, не молчание.
- [ ] Транзакции финишируются единой идиомой: named err + `defer FinishTransaction(err, tx)`; упавший rollback комбинируется с оригинальной ошибкой (`multierr`).
- [ ] Storage-структура приватна для адаптера; домен восстанавливается только фабрикой `UnmarshalFromDatabase`, прогоняющей данные через валидирующие конструкторы.
- [ ] `sql.ErrNoRows` → доменная `NotFoundError`; уникальные констрейнты → доменные sentinel-ы; драйверные ошибки выше repo не протекают.
- [ ] In-memory: карта **значений** + `RWMutex`; Get возвращает адрес копии; update — копия + запись только при успехе.
- [ ] Один shared suite на все реализации; обязательны rollback-тест и race-тест («N горутин, `close(start)`, ровно один победитель»); прогон — с `-race`.
- [ ] Интеграционные события публикуются в outbox **той же транзакцией**, что и persist агрегата (детали — в главе о событиях).

## Ссылки

- [ARCHITECTURE.md §2](../ARCHITECTURE.md) — агрегат `auction.Auction`, контракт Repository, семантика фасадных команд саги.
- [ARCHITECTURE.md §7](../ARCHITECTURE.md) — схема БД: `version`, `auctions_relist_of_uq`, append-only `auction_bids`, правила маппинга.
- [BOOK_AUDIT.md, правила 15–20](../BOOK_AUDIT.md) — контракт ревью раздела Repository: интерфейс в домене, updateFn, «глупый» repo, finishTransaction, маппинг ошибок, in-memory.
- [TEXTBOOK.md, глава 5](../TEXTBOOK.md) — краткая «почему»-версия этой главы и золотое правило 5: «Репозиторий глуп; транзакции и локи — деталь адаптера».
- Код: [domain/auction/repository.go](../../internal/auction/domain/auction/repository.go) · [adapters/auction_pg_repository.go](../../internal/auction/adapters/auction_pg_repository.go) · [adapters/auction_inmem_repository.go](../../internal/auction/adapters/auction_inmem_repository.go) · [common/postgres/postgres.go](../../internal/common/postgres/postgres.go) · [adapters/repository_suite_test.go](../../internal/auction/adapters/repository_suite_test.go) · [adapters/pg_integration_test.go](../../internal/auction/adapters/pg_integration_test.go)
