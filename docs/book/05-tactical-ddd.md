# Глава 5. Тактический DDD: агрегаты, инварианты, value objects

## Зачем читать эту главу

Стратегический DDD (главы 2–3) ответил на вопрос «где»: bounded contexts, модули монолита, события между ними. Эта глава — про то, что происходит **внутри** границы: как устроен код, который непосредственно держит бизнес-правила.

Сразу честное предупреждение: ни одного нового фреймворка и ни одной библиотеки здесь не будет. Весь тактический DDD в Go — это дисциплина работы с приватными полями, конструкторами и методами. Звучит буднично, но именно эта дисциплина решает, во что превратится проект через два года: в систему, где правило ставки находится и меняется за минуту, — или в систему, где никто уже не помнит, в каком из одиннадцати хендлеров живёт проверка «продавец не ставит на свой лот». Обе системы, что характерно, начинаются с одинаково зелёного CI.

Цель главы — не «запомнить приёмы», а уметь их защитить на ревью. Дочитав, вы сможете обосновать: почему все поля агрегата приватные, почему порядок guard-ов — часть контракта, почему `panic` в `default` закрытого enum — правильный код (да, вас учили обратному) и почему `MinimalNextBid` — функция пакета, а не метод «сервиса».

## Проблема: анемичная модель и «стена if-ов»

Посмотрим на типичный Go-сервис без тактического DDD (псевдокод, **не из Molot** — это анти-пример):

```go
// Анти-паттерн: анемичная модель
type Auction struct {
    ID         string
    SellerID   string
    Status     string // "listed" | "closed" | ???
    LeadingBid *Bid   // nil = ставок нет
    EndsAt     time.Time
}

// ... а правила — в хендлере:
func (h *Handler) PlaceBid(w http.ResponseWriter, r *http.Request) {
    if a.Status != "listed" { /* 409 */ }
    if a.SellerID == req.UserID { /* 403 */ }
    if a.LeadingBid != nil && req.Amount <= a.LeadingBid.Amount { /* 409 */ }
    // ... ещё шесть if-ов, и в gRPC-хендлере — их копия
}
```

Выглядит безобидно — большинство сервисов так и написано, и первые месяцы это даже работает. Боль приходит по нарастающей.

Сначала вы замечаете, что **правило размазано по вызывающим**. Проверку «лидер не перебивает сам себя» нужно повторить в HTTP-хендлере, в gRPC-хендлере, в воркере и в тесте. Один код-путь забыл — и невалидное состояние уехало в базу. Это не гипотеза, а арифметика: каждый новый вызывающий — ещё одна вероятность забыть.

Потом выясняется, что **невалидное состояние представимо**. `Auction{Status: "listed", LeadingBid: &Bid{Amount: -5}}` компилируется и сохраняется. Валидация существует только как добрая воля того, кто заполнял struct, — а добрая воля, в отличие от типов, не масштабируется.

Дальше — **строковые статусы и указатели-флаги**. `a.Status == "lsited"` — опечатка, которую компилятор не видит. `LeadingBid *Bid` порождает nil-паники и вечный вопрос «nil — это „нет ставок" или „забыли загрузить"?».

И финальный счёт: **тестировать можно только через стек**. Раз правила живут в хендлере, для проверки одного if-а нужны HTTP-запрос, мок репозитория и фикстура базы. Юнит-тест правила в чистом виде писать просто не из чего.

Анемичная модель — это transaction script, который притворяется объектной моделью. Для тривиального CRUD это нормально (см. «Трейдоффы»). Для домена с деньгами и конкуренцией — гарантированная деградация: каждое новое правило увеличивает стену if-ов мультипликативно, по числу вызывающих.

Чтобы стена не выросла, нужны четыре опоры. Разберём их по очереди — а потом посмотрим, как каждая выглядит в живом коде.

## Теория: четыре опоры тактического DDD

### Always-valid: невалидное состояние непредставимо

Центральный принцип, из которого выводится всё остальное: объект домена **не может существовать** в невалидном виде — ни сразу после создания, ни после любого перехода. Механика держится на трёх вещах: **приватные поля** — извне состояние не присвоить; **валидирующий конструктор** `NewX(...) (*X, error)` — единственная дверь внутрь, каждый пустой или нулевой аргумент отклоняется; **behavior-методы** — каждый переход состояния сам проверяет свой guard, так что «проверить» и «изменить» — одна неделимая операция.

Следствие, которое легко недооценить: вызывающему **нечего забыть**. У хендлера физически нет способа сделать ставку мимо проверки окна — нет ни сеттера `SetLeadingBid`, ни публичного поля. Класс багов «забыли проверить» не ловится на ревью — он **не существует**. Почувствуйте разницу: вы не стали внимательнее, вы убрали саму возможность ошибиться.

### Агрегат = граница согласованности и граница транзакции

Aggregate — это кластер объектов, который меняется как единое целое: одна транзакция — один агрегат (BOOK_AUDIT, §3). Всё, что должно быть согласовано *немедленно* (инвариант), живёт внутри границы; всё, что может быть согласовано *в итоге*, — снаружи, через события.

Тонкость, которую упускают чаще всего: граница согласованности и объём данных в памяти — **разные вещи**. Агрегат обязан *гарантировать* инвариант над данными, но не обязан *держать* все эти данные загруженными. Если сейчас это звучит абстрактно — потерпите до разбора ADR-0003 ниже: Molot эксплуатирует это различие в полную силу.

### Value objects: тип как валидатор

Value Object — immutable-значение без идентичности, сравниваемое по содержимому. В Go это struct с приватными полями, конструктором и методами-операциями. Главный фокус VO в том, что он переносит проверку из «места использования» в «место создания»: если у вас в руках `Money` — валюта уже валидна, сумма уже неотрицательна. Тип — это доказательство. Работает это как стерильность ампулы: её гарантирует завод при запайке, а не медсестра у постели больного — потому у постели её никто и не перепроверяет.

Идиома проекта: **zero value = «не задано»**, проверяется через `IsZero()`. Никаких `*Money` с nil-семантикой — указатель-флаг порождает три состояния (nil, zero, значение) там, где смысла два.

### Ошибки — часть языка домена

Каждое нарушение инварианта — именованная sentinel-ошибка: `var ErrBidBelowMinimum = errors.New(...)`. Не думайте об этом как об «обработке ошибок» — это **словарь**: домен сообщает вызывающему *что именно* не так, на языке бизнеса, и вызывающий ветвится через `errors.Is`, не перепроверяя то, что домен уже гарантировал (BOOK_AUDIT, правило 10).

Теории достаточно. Дальше — решения Molot, по одному, и у каждого будет ответ «почему именно так».

## Как это сделано в Molot

### Граница Auction: инварианту нужна голова, истории — много (ADR-0003)

Самое интересное решение проекта — граница агрегата `Auction`, и начинается оно с конфликта. История ставок «горячего» лота не ограничена (тысячи строк), а **все** инварианты ставки зависят только от «головы» состояния — текущего лидера, окна, шага. Грузить историю в память — O(n) и рост contention; вынести `Bid` в отдельный агрегат — потерять атомарность проверки «ставка против актуального лидера». Куда ни шагни — больно.

Выход: полная история ставок входит в **границу согласованности** (пишется в той же транзакции), но **не загружается в память**. Агрегат хранит топ-2 ставки денормализованно:

```go
// internal/auction/domain/auction/auction.go
type Auction struct {
	id               AuctionID
	seller           SellerID
	lot              Lot
	startPrice       Money
	increment        Money
	reserve          ReservePrice // zero = no reserve; hidden from events/API
	window           BiddingWindow
	antiSnipe        AntiSnipePolicy // snapshot taken at listing time
	verifyAbove      Money           // snapshot of verified_bid_threshold at listing time
	extensionsUsed   int
	status           Status
	outcome          Outcome // zero until closed
	leadingBid       Bid     // zero value = no bids (no pointer flags)
	runnerUpBid      Bid
	winnerReassigned bool // "AwardToRunnerUp already done" marker for facade idempotency
	bidCount         int
	relistOf         AuctionID // zero = original listing
	relistGen        int       // 0 | 1 — cap of automatic relists
	settled          bool      // settlement finished (with success or failure)
	version          int64     // mapping only; the adapter increments it
	events           []DomainEvent
}
```

Прочитайте этот struct сверху вниз — он работает как конспект всей главы: VO вместо примитивов, zero value вместо указателей-флагов (`leadingBid Bid` с комментарием «no pointer flags»), снапшоты политик (`antiSnipe`, `verifyAbove`), накопитель `events` в хвосте. Дальше механика: `PlaceBid` возвращает принятую `Bid` — и репозиторий в **одной транзакции** аппендит её в `auction_bids` (append-only, никогда UPDATE) и обновляет snapshot-строку `auctions` с `version+1`. Загрузка агрегата — O(1) независимо от размера истории; optimistic locking — по одной маленькой строке. Runner-up хранится не «на всякий случай»: он нужен саге расчётов для second chance, то есть это часть инварианта, а не кэш. Чтение истории — забота read-стороны: query `BidHistory` ходит прямо в `auction_bids`.

Это и есть практический смысл фразы «граница согласованности ≠ объём памяти». Подробный разбор альтернатив — в [ADR-0003](../adr/0003-auction-aggregate-boundary.md).

### Валидирующий конструктор: единственная дверь

Поля приватные (правило 8) — значит, экземпляр снаружи пакета можно получить ровно двумя способами: фабрика `New` (бизнес-создание, с событием) и `UnmarshalFromDatabase` (регидрация адаптером, без события). Третьего пути нет — компилятор не позволит.

```go
// internal/auction/domain/auction/auction.go
func New(
	id AuctionID,
	seller SellerID,
	lot Lot,
	startPrice, increment Money,
	reserve ReservePrice,
	window BiddingWindow,
	rules ListingRules,
	now time.Time,
) (*Auction, error) {
	if id.IsZero() || seller.IsZero() {
		return nil, ErrInvalidID
	}
	if lot.IsZero() {
		return nil, ErrInvalidLot
	}
	// ... каждый аргумент проверяется; фрагмент сокращён ...
	if startPrice.Currency() != rules.Currency() {
		return nil, ErrUnsupportedCurrency
	}
	if increment.Currency() != startPrice.Currency() {
		return nil, ErrCurrencyMismatch
	}
	if !window.EndsAt().After(now) {
		return nil, ErrInvalidBiddingWindow
	}

	a := &Auction{
		id:          id,
		seller:      seller,
		lot:         lot,
		startPrice:  startPrice,
		increment:   increment,
		reserve:     reserve,
		window:      window,
		antiSnipe:   rules.AntiSnipe(),
		verifyAbove: rules.VerifyAbove(),
		status:      StatusListed,
		version:     1,
	}
	a.record(AuctionListed{OccurredAt: now})
	return a, nil
}
```

Обратите внимание на два неочевидных решения. Первое: `now time.Time` — параметр, а не `time.Now()` внутри. Время — зависимость, и домен получает её явно (глава 6); заодно тесты получают детерминизм бесплатно. Второе: `rules ListingRules` — это **снапшот платформенной политики**, и о нём ниже отдельный разговор.

> **Совет из практики.** Когда вам приносят на ревью новый агрегат, не начинайте с методов. Начните с вопроса: «можно ли создать этот объект в обход фабрики?» Один экспортированный struct-литерал в тестах, один «временный» сеттер — и вся конструкция always-valid превращается в декорацию: выглядит как гарантия, работает как пожелание. Сначала проверьте дверь, потом мебель.

### PlaceBid: каскад guard-ов, где порядок — тоже контракт

Создать валидный аукцион — полдела; теперь его нужно валидно менять. Центральный behavior-метод — `PlaceBid`, и его guard-каскад стоит прочитать медленно:

```go
// internal/auction/domain/auction/auction.go — PlaceBid
	// Guard cascade — order is fixed by §2.1.
	if a.status != StatusListed || !a.window.IsOpenAt(now) {
		return Bid{}, ErrAuctionNotOpen
	}
	if b.ID().UUID() == a.seller.UUID() {
		return Bid{}, ErrSellerCannotBid
	}
	if !a.leadingBid.IsZero() && a.leadingBid.Bidder() == b.ID() {
		return Bid{}, ErrLeaderCannotOutbidSelf
	}
	if amount.Currency() != a.startPrice.Currency() {
		return Bid{}, ErrCurrencyMismatch
	}
	if meetsMin, err := amount.GTE(MinimalNextBid(*a)); err != nil || !meetsMin {
		return Bid{}, ErrBidBelowMinimum
	}
	if !b.Verified() {
		needsVerification, err := amount.GTE(a.verifyAbove)
		if err != nil || needsVerification {
			return Bid{}, ErrVerificationRequired
		}
	}
```

Порядок зафиксирован документацией (ARCHITECTURE.md §2.1) и тестами — и это не педантизм, а три отдельных соображения.

Первое: **ошибка — наблюдаемое поведение.** Продавец, ткнувший в собственный закрытый лот, получит `ErrAuctionNotOpen`, а не `ErrSellerCannotBid` — потому что guard состояния стоит раньше guard-а актора. Поменяли порядок — поменяли ответ API; клиенты и тесты это заметят. Порядок каскада — такой же контракт, как сигнатура.

Второе: **каждый guard опирается на предыдущие.** Проверка валюты стоит *до* `GTE(MinimalNextBid(...))` — поэтому сравнение сумм уже не может упасть на mismatch: к пятому guard-у валюта доказана четвёртым. Каскад — это цепочка усиливающихся предусловий, а не набор независимых проверок.

Третье: **один guard — одна ошибка.** Поддержка и тесты получают детерминированную диагностику: для любой комбинации нарушений ответ предсказуем.

После каскада — переход, **атомарно с проверкой** (правило 9): и продвижение лидера, и анти-снайп-продление, и запись событий происходят в том же вызове. Между «проверили» и «изменили» не может вклиниться ничей код:

```go
// internal/auction/domain/auction/auction.go — PlaceBid, переход
	outbid := a.leadingBid
	a.runnerUpBid = a.leadingBid
	a.leadingBid = bid
	a.bidCount++

	extended := false
	if a.antiSnipe.TriggersAt(now, a.window.EndsAt()) && a.extensionsUsed < a.antiSnipe.MaxExtensions() {
		a.window = a.window.ExtendedBy(a.antiSnipe.Extension())
		a.extensionsUsed++
		extended = true
		a.record(AuctionExtended{NewEndsAt: a.window.EndsAt(), OccurredAt: now})
	}

	a.record(BidPlaced{
		Bid:        bid,
		Outbid:     outbid,
		NewEndsAt:  a.window.EndsAt(),
		Extended:   extended,
		OccurredAt: now,
	})
	return bid, nil
```

Здесь стоит разглядеть два момента. `outbid` запоминается до продвижения лидера — событие `BidPlaced` обязано сообщить, кого перебили. А анти-снайп-продление порождает собственное `AuctionExtended` в той же операции — наружу уходит согласованная пара фактов, а не два события из разных транзакций, между которыми кто-то успел бы увидеть промежуточный мир.

### Предикаты вместо геттеров, PullDomainEvents вместо публичного слайса

Раз поля закрыты, встаёт вопрос: а как с агрегатом вообще разговаривать снаружи? Публичный API — behavior-методы (`PlaceBid`, `Cancel`, `Close`, `AwardToRunnerUp`...), **предикаты** (`IsOpenAt(now)`, `HasBids()`) и типизированные геттеры для маппинга (`Status()`, `EndsAt()`). Сеттеров нет вообще; геттеров вида `GetX` — тоже (правило 9). Разница между предикатом и геттером принципиальна: `a.HasBids()` инкапсулирует знание «ставки считаются по bidCount», а `a.BidCount() > 0` в вызывающем коде — это утёкшая деталь представления, которая сломается при рефакторинге.

События копятся внутри и **дренируются** адаптером:

```go
// internal/auction/domain/auction/auction.go
// PullDomainEvents drains recorded events for the persisting adapter.
func (a *Auction) PullDomainEvents() []DomainEvent {
	events := a.events
	a.events = nil
	return events
}
```

Шесть строк, но семантика выверена: «pull = забрать и очистить» гарантирует, что событие уйдёт в outbox ровно из той транзакции, которая совершила переход, — и не уйдёт повторно при следующем сохранении. Публичный слайс `Events []DomainEvent` дал бы вызывающим и дубли, и возможность дописать событие мимо домена.

### Value objects: Money, ReservePrice, BiddingWindow, AntiSnipePolicy

Теперь спустимся на уровень ниже — к кирпичам, из которых агрегат сложен. `Money` — int64 в минорных единицах плюс валюта. Никаких float (глава 10 — про деньги подробно), никаких операций между валютами:

```go
// internal/auction/domain/auction/money.go
type Money struct {
	amount   int64
	currency Currency
}

func NewMoney(amountMinor int64, currency Currency) (Money, error) {
	if currency.IsZero() {
		return Money{}, ErrInvalidCurrency
	}
	if amountMinor < 0 {
		return Money{}, ErrNegativeAmount
	}
	return Money{amount: amountMinor, currency: currency}, nil
}

// Add returns m+o, failing on a currency mismatch.
func (m Money) Add(o Money) (Money, error) {
	if m.currency != o.currency {
		return Money{}, ErrCurrencyMismatch
	}
	return Money{amount: m.amount + o.amount, currency: m.currency}, nil
}

// GTE reports whether m >= o, failing on a currency mismatch.
func (m Money) GTE(o Money) (bool, error) {
	if m.currency != o.currency {
		return false, ErrCurrencyMismatch
	}
	return m.amount >= o.amount, nil
}
```

Главное здесь то, чего сделать **нельзя**: сравнить «сырые» int64 двух разных валют через этот тип невозможно — операция сама несёт правило. И да, у billing-контекста свой `Money`, дубль осознанный (BOOK_AUDIT, правило 4): общий бизнес-тип в shared kernel сцепил бы контексты сильнее, чем экономил бы строк.

`ReservePrice` показывает, как VO превращает «if с нюансами» в слово предметной области:

```go
// internal/auction/domain/auction/reserve.go
// MetBy reports whether amount satisfies the reserve. No reserve is
// met by any amount; a currency mismatch never satisfies it.
func (r ReservePrice) MetBy(amount Money) bool {
	if r.IsZero() {
		return true
	}
	ok, err := amount.GTE(r.price)
	if err != nil {
		return false
	}
	return ok
}
```

Вызывающий код читается как ТЗ: `reserve.MetBy(leadingBid.Amount())`. Семантика краевых случаев («нет резерва — пройден любой суммой», «чужая валюта — не пройден никогда») зашита в **одно** место. Заметьте и второй слой дизайна: сам резерв никогда не покидает агрегат — события и `ClosingResult` несут только производные (`RunnerUpQualifies bool`), сага получает готовый вердикт, не зная скрытую цену.

`AntiSnipePolicy` и `BiddingWindow` — пара VO, на которых держится анти-снайпинг:

```go
// internal/auction/domain/auction/antisnipe.go
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

Присмотритесь к формуле: `!now.Before(...)` вместо `now.After(...)` — граница включающая, ставка ровно в момент `endsAt-window` уже триггерит продление. Такие мелочи и есть аргумент держать правило в одном VO, а не рассыпанным по хендлерам: краевой случай решён один раз и навсегда.

`BiddingWindow.ExtendedBy(d)` возвращает **новое** окно (VO immutable), сохраняя `originalEndsAt` для прозрачности — пользователь видит и исходный, и продлённый дедлайн.

### Закрытые enum: switch + panic в default

Перечислимые состояния — не строки и не iota-константы, а value-object типы с приватным полем (правило 12):

```go
// internal/auction/domain/auction/status.go
type Status struct {
	s string
}

var (
	StatusListed    = Status{"listed"}
	StatusCancelled = Status{"cancelled"}
	StatusClosed    = Status{"closed"}
)

func NewStatusFromString(s string) (Status, error) {
	switch s {
	case StatusListed.s, StatusCancelled.s, StatusClosed.s:
		return Status{s}, nil
	}
	return Status{}, fmt.Errorf("%w: %q", ErrInvalidStatus, s)
}
```

Поле приватное ⇒ снаружи пакета значение `Status` можно получить только из трёх объявленных переменных или конструктора. Enum **закрыт** — и поэтому `switch` по нему обязан паниковать в `default`:

```go
// internal/auction/domain/auction/auction.go — Cancel
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
```

Стоп, скажете вы, — «в библиотечном коде не паникуют». Верно, но здесь паника правильна, потому что эта ветка **недостижима пользовательским вводом**. Попасть в default можно одним способом: разработчик добавил четвёртый статус и не обновил switch. Это не business outcome, который надо вернуть как error и обработать, — это баг, и лучшее, что можно сделать, — упасть громко в точке бага, а не молча проглотить неизвестное состояние аукциона с деньгами. Тот же принцип — в `MinimalNextBid` (см. ниже) и в каждом behavior-методе `Invoice`.

> **Нюанс.** Граница между error и panic проходит по источнику значения. Строка пришла извне — из БД, из запроса, из конфига — это данные внешнего мира, и `NewStatusFromString` честно возвращает error. Но если в руках уже значение типа `Status`, а switch его не узнал — данные ни при чём: это разработчик добавил статус и не дописал ветку. Паникуйте на нарушенных инвариантах кода, возвращайте ошибки на фактах внешнего мира — и спор «panic или error» в команде закончится за пять минут.

### Снапшот политик при листинге: правила лота детерминированы во времени

Порог верификации и анти-снайп-политика — платформенный конфиг. Наивная реализация читала бы конфиг в `PlaceBid` — и правила лота менялись бы посреди торгов вместе с конфигом. Molot снимает снапшот **один раз**, фабрикой, при листинге:

```go
// internal/auction/domain/auction/listing_rules.go
// ListingRules is the platform policy snapshotted onto the aggregate
// once, by the factory, at listing time: the platform currency, the
// verified-bid threshold and the anti-snipe policy. PlaceBid never
// reads platform config (§2.1).
type ListingRules struct {
	currency    Currency
	verifyAbove Money
	antiSnipe   AntiSnipePolicy
}
```

Бизнес-мотивация прямая: участник, ставивший на лот при одних правилах, не должен обнаружить посреди торгов другие. Смена платформенной политики действует на **новые** листинги; идущие аукционы доигрываются по правилам, действовавшим на момент листинга — как договор, условия которого фиксируются при подписании, а не перечитываются из текущего прейскуранта. Бонусы: `PlaceBid` остаётся чистой функцией состояния агрегата (нет скрытой зависимости от конфига), а спор «по каким правилам закрылся лот №N» решается чтением строки агрегата, не археологией истории конфига. Тот же приём — в billing: `NewInvoice` снапшотит `commission` и `total` из `CommissionPolicy` на момент выставления счёта.

> **Совет из практики.** Снапшот политики — приём не только для аукционов. Любой долгоживущий процесс, пересекающийся с «горячим» конфигом — заказ с условиями доставки, подписка с тарифом, кредитная заявка со ставкой, — рано или поздно порождает вопрос «по каким правилам он шёл». Если правила скопированы в агрегат при старте, ответ — один SELECT. Если нет — раскопки по истории деплоев конфиг-сервиса, и хорошо ещё, если она велась.

### Sentinel-ошибки и маппинг в slug на границе

Вернёмся к словарю домена. Весь набор нарушений — экспортируемые package-level sentinels (правило 10):

```go
// internal/auction/domain/auction/errors.go
var (
	// PlaceBid guard cascade (§2.1, order fixed).
	ErrAuctionNotOpen         = errors.New("auction is not open for bidding")
	ErrSellerCannotBid        = errors.New("seller cannot bid on own auction")
	ErrLeaderCannotOutbidSelf = errors.New("current leader cannot outbid self")
	ErrCurrencyMismatch       = errors.New("currency mismatch")
	ErrBidBelowMinimum        = errors.New("bid is below the minimal next bid")
	ErrVerificationRequired   = errors.New("bid amount requires a verified bidder")
	// ...
)
```

Домен говорит точную причину — **всегда**. Интерпретация («а для нас это no-op» или «наружу отдадим 409») — забота вызывающего слоя. Command handler транслирует sentinels в ports-agnostic slug-ошибки ровно в одном месте:

```go
// internal/auction/app/command/place_bid.go
func mapPlaceBidError(err error) error {
	if mapped, ok := mapNotFound(err); ok {
		return mapped
	}
	switch {
	case errors.Is(err, auction.ErrAuctionNotOpen):
		return errs.NewConflictError("auction-not-open").WithCause(err)
	case errors.Is(err, auction.ErrSellerCannotBid):
		return errs.NewForbiddenError("seller-cannot-bid").WithCause(err)
	case errors.Is(err, auction.ErrBidBelowMinimum):
		return errs.NewConflictError("bid-below-minimum").WithCause(err)
	case errors.Is(err, auction.ErrVerificationRequired):
		return errs.NewForbiddenError("verification-required").WithCause(err)
	// ...
	default:
		return err
	}
}
```

Смотрите, что делает эта функция: трёхзвенная цепочка `sentinel → slug → HTTP-статус` держит каждый слой на своём языке — домен не знает про 409, ports не знают про инварианты. Для ошибок, которым нужен контекст (кто и чьё пытался трогать), вместо sentinel — типизированная структура: `ForbiddenAuctionManagementError{Actor, Owner}` несёт обоих принципалов для аудит-лога, оставаясь при этом обычной ошибкой для `errors.As`.

### Guard-таблица Invoice: исчерпывающие переходы как способ мышления

Агрегат `Invoice` (billing) компактнее `Auction`, но демонстрирует приём, который стоит забрать в любой проект: **таблица переходов покрывает каждую клетку**. Для каждого метода и каждого текущего статуса ответ определён заранее (ARCHITECTURE.md §2.2):

| Метод \ статус | Pending | Paid | Expired | Voided |
|---|---|---|---|---|
| `MarkPaid` | → Paid | `ErrInvoiceAlreadyPaid` | `ErrInvoiceExpired` | `ErrInvoiceVoided` |
| `Expire` | now>=dueAt → Expired; иначе `ErrInvoiceNotDue` | `ErrInvoiceAlreadyPaid` → nil (воркер) | `ErrInvoiceAlreadyExpired` → nil | `ErrInvoiceVoided` → nil |
| `Void` | → Voided | `ErrInvoiceAlreadyPaid` (**НЕ** no-op: оплата победила) | `ErrInvoiceAlreadyExpired` → nil | `ErrInvoiceAlreadyVoided` → nil |

Код — буквальная транскрипция строки таблицы, включая панику на невозможном статусе:

```go
// internal/billing/domain/invoice/invoice.go
// Void transitions Pending → Voided (saga: the runner-up declined the
// second-chance offer). ErrInvoiceAlreadyPaid is NOT a no-op for
// callers: the payment won and the decline must fail (§2.2).
func (i *Invoice) Void(now time.Time) error {
	switch i.status {
	case StatusPending:
		i.status = StatusVoided
		i.record(InvoiceVoided{InvoiceID: i.id, OccurredAt: now.UTC()})
		return nil
	case StatusPaid:
		return ErrInvoiceAlreadyPaid
	case StatusExpired:
		return ErrInvoiceAlreadyExpired
	case StatusVoided:
		return ErrInvoiceAlreadyVoided
	default:
		panic("invoice: unknown status " + i.status.s)
	}
}
```

Сверьте код с таблицей строку за строкой — совпадение буквальное, и это не совпадение, а метод. Две детали, ради которых таблица существует. Первая: пометки «→ nil» — это интерпретация **вызывающего** (воркер экспирации трактует «уже оплачен» как no-op), домен же всегда возвращает точную причину; одна и та же ошибка `ErrInvoiceAlreadyPaid` для `Expire` — no-op, а для `Void` — жёсткий отказ, потому что оплата выиграла гонку у decline. Вторая: исчерпывающая таблица — рабочий инструмент мышления, как предполётный чек-лист: ценность не в том, что пилот не умеет выпускать шасси, а в том, что ни один пункт нельзя пропустить молча. Заполняя клетку «Void × Expired», вы *обязаны* решить, что значит «runner-up отклонил оферту по уже истёкшему счёту», — до того, как это решит за вас прод в три часа ночи. Пустых клеток не бывает: либо переход, либо sentinel, либо паника.

> **Где вы на это наступите.** Гонка «оплата против отмены» — не учебная страшилка. Вебхук PSP об успешном платеже и клик пользователя «отказаться» регулярно приходят в одну и ту же секунду — и если семантика этой пары не решена заранее, её решит порядок доставки пакетов: в одном случае выигрывает оплата, в другом — отказ, а саппорт получает тикет «деньги списали, лот не мой». Таблица переходов — это решение, принятое днём на свежую голову, а не в три ночи посреди инцидента.

### Stateless-вычисления — функции пакета: «не делай Java в Golang»

Последний приём — самый простой и самый часто нарушаемый. Расчёты, не меняющие состояние, — это **не** методы и **не** «доменные сервисы» с интерфейсом и конструктором. Это просто функции доменного пакета (правило 13):

```go
// internal/auction/domain/auction/auction.go
// MinimalNextBid is the lowest acceptable bid: the start price when
// there are no bids, otherwise leading + increment.
func MinimalNextBid(a Auction) Money {
	if a.leadingBid.IsZero() {
		return a.startPrice
	}
	minNext, err := a.leadingBid.Amount().Add(a.increment)
	if err != nil {
		// Same-currency is a construction invariant; reaching here is
		// a programmer error, not a business outcome.
		panic("auction: leading bid and increment currency diverged: " + err.Error())
	}
	return minNext
}
```

```go
// internal/billing/domain/invoice/commission.go
// CommissionFor is a stateless calculation, deliberately separate from
// aggregate mutation (BOOK_AUDIT rule 13): the platform commission for
// a given hammer price under policy p.
func CommissionFor(hammer Money, p CommissionPolicy) Money {
	return hammer.MulBasisPoints(p.basisPoints)
}
```

Обе функции — формулы в чистом виде: вход, выход, ноль зависимостей. Разделение расчёта и мутации — это SRP плюс Command-Query Separation на уровне домена: `MinimalNextBid` зовут и guard внутри `PlaceBid`, и query «карточка лота» для подсказки в UI — одна формула, ноль дублей, ноль моков в тестах. Альтернатива в духе Java — `type BidPricingService struct{}` с конструктором, интерфейсом и инжекцией — добавила бы три файла и ни одного свойства: у функции нет состояния, ей нечего инжектить. Туда же относится и авторизация: `CanSellerManageAuction(s SellerID, a Auction) error` — чистая функция (правило 22), которую repo вызывает внутри транзакции.

И снова паника в `MinimalNextBid`: одинаковость валют `leadingBid` и `increment` — конструкционный инвариант (доказан в `New` и в guard-каскаде). Если он нарушен — в системе баг, и честная паника лучше «тихого» error, который кто-нибудь когда-нибудь обработает как бизнес-кейс.

## Трейдоффы и альтернативы

Ни одно из решений выше не бесплатно. Вот альтернативы, которые лежали на столе, и почему они проиграли.

**Все ставки в памяти агрегата** — каноничный «чистый DDD», так написано в книжках. Отклонено (ADR-0003): память и contention растут с историей, а инварианту вся история не нужна. Цена решения Molot — денормализация топ-2 (дублирование данных лога); принята осознанно: снапшот восстановим из `auction_bids`.

**Bid как отдельный агрегат** — снимает вопрос размера, но инвариант «ставка против актуального лидера и окна» требует одной транзакции и одного лока. Между двумя агрегатами осталась бы только eventual consistency — для гонки двух ставок это дыра, а не трейдофф.

**Event sourcing для Auction** — соблазнительно: append-only лог уже есть, рука сама тянется. Отложено: добавляет инфраструктуру и кривую обучения, аудит уже обеспечен логом ставок, а переход возможен позже за тем же интерфейсом `Repository`. Соблазн — плохой архитектурный аргумент.

**Анемичная модель + transaction script** — легитимна для тривиального CRUD (правило 33: слои не применяются к справочникам и auth, исключение документируется). Тактический DDD — инструмент пропорциональный: разворачивать его на таблице «список валют» — та же ошибка инженерной избыточности, что и его отсутствие на агрегате с деньгами.

**Общий Money в shared kernel** — экономит ~60 строк на контекст, но создаёт ось сцепления: изменение под нужды billing (например, поддержка отрицательных сумм для refund) молча меняет семантику в auction. Molot дублирует VO по контекстам (BOOK_AUDIT, правило 4) — DRY применяется к поведению, не к данным.

**Геттеры: прагматизм против пуризма.** «No getters» в радикальной трактовке делает невозможным маппинг в storage-модель. Molot различает: сеттеров нет вообще, `GetX`-имён нет, но типизированные геттеры для маппинга (`Status()`, `Version()`) и предикаты для логики (`IsOpenAt`) — есть. Линия проведена не по форме, а по назначению: геттер для адаптера — да, геттер для ветвления бизнес-логики снаружи домена — дефект (правило 14: бизнес-`if` в app-слое переезжает в домен).

## Типичные ошибки

Десять граблей, на которые наступают чаще всего. Большинство выглядит как мелочь — тем и опасны.

1. **Валидация только на входе HTTP.** DTO-валидатор проверил JSON — и все выдохнули. А воркер и сага зовут домен напрямую, мимо валидатора. Always-valid требует валидации в конструкторе домена; транспортная валидация — лишь ранний отказ, вежливость к клиенту, а не гарантия.
2. **Сеттер «только для тестов».** `SetStatus(s)` с комментарием «test only» разрушает всю конструкцию: невалидное состояние снова представимо, и неважно, кто обещал «звать только из тестов». Правильно — тестовые фикстуры через публичные фабрики (`fixtures_test.go` в пакете) или `UnmarshalFromDatabase`.
3. **Перепроверка инварианта вызывающим.** `if auction.IsOpenAt(now) { auction.PlaceBid(...) }` — TOCTOU в миниатюре и дубль правила. Зовите behavior-метод и ветвитесь по ошибке: домен уже всё проверил, атомарно.
4. **bool вместо sentinel.** `CanBid() bool` теряет причину отказа; вызывающий начинает гадать и дублировать диагностику. Guard возвращает `error` с точным именем.
5. **Открытый enum.** `type Status string` + константы: любая строка приводится к типу, switch без default молча пропускает мусор. Закрытый тип с приватным полем + паника в default.
6. **Указатель как флаг отсутствия.** `leadingBid *Bid` вместо zero value + `IsZero()`: nil-паники, три состояния вместо двух и одни и те же вопросы на каждом ревью.
7. **Доменное событие при no-op.** Повторный `ConfirmSettlement` записал второе `SaleSettled` — в outbox дубль, у подписчиков двойной эффект. Событие записывает только транзакция, реально совершившая переход; повтор получает sentinel.
8. **Чтение конфига внутри behavior-метода.** Правила лота начинают дрейфовать вместе с конфигом; тест требует мок конфига. Снапшот политики при создании агрегата.
9. **«Доменный сервис» для формулы.** Объект без состояния с одним методом — это функция, которую обернули в церемонию. Function-first; объект — когда появится состояние или потребность в полиморфизме.
10. **Sentinel наружу как есть.** HTTP-хендлер сравнивает `errors.Is(err, auction.ErrBidBelowMinimum)` — транспорт сцеплен с доменом через слой. Маппинг в slug — в command handler, один раз.

## Чек-лист

Прогоните по этому списку каждый агрегат — свой или тот, что принесли на ревью:

- [ ] Все поля агрегатов и VO — unexported; единственные пути создания — валидирующая фабрика и `UnmarshalFromDatabase` (правила 7, 8).
- [ ] Каждый behavior-метод: guard инварианта + переход + запись события — атомарно, в одном вызове (правило 9).
- [ ] Порядок guard-ов зафиксирован документацией и тестами; изменение порядка осознаётся как изменение контракта.
- [ ] Нарушения инвариантов — экспортируемые sentinels; вызывающие ветвятся через `errors.Is` и не перепроверяют гарантии домена (правило 10).
- [ ] Ошибкам, которым нужен контекст принципалов, — типизированные структуры (`ForbiddenXError{Actor, Owner}`).
- [ ] Маппинг sentinel → slug — в command handler; домен не знает HTTP-статусов, ports не знают инвариантов.
- [ ] Перечислимые состояния — закрытые VO-типы: приватное поле, конструктор из сырого значения, `IsZero()`, паника в `default` switch (правило 12).
- [ ] «Не задано» — zero value + `IsZero()`, не указатель.
- [ ] Money: минорные единицы int64 + валюта; операции проверяют валюту; дубль типа по контекстам — осознан.
- [ ] Политики платформы снапшотятся на агрегат при создании; behavior-методы не читают конфиг.
- [ ] Для каждого агрегата с lifecycle — исчерпывающая guard-таблица: каждая пара (метод × статус) имеет определённый ответ; «→ nil» — решение вызывающего, не домена.
- [ ] Stateless-расчёты — функции пакета, отдельно от мутаций; авторизационные правила — чистые функции `CanX(actor, agg) error` (правила 13, 22).
- [ ] События дренируются через `PullDomainEvents`; повторный вызов идемпотентной команды не порождает событие.
- [ ] Время и идентификаторы — параметры behavior-методов, не вызовы `time.Now()`/`uuid.New()` внутри домена.
- [ ] Граница агрегата выбрана по инварианту, а не по объёму данных: что должно быть согласовано немедленно — внутри; объём в памяти — отдельное решение.

## Ссылки

- [ARCHITECTURE.md §2 «Агрегаты и инварианты»](../ARCHITECTURE.md) — полная спецификация `Auction` (§2.1) и `Invoice` (§2.2): guard-каскад, семантика фасадных команд саги, guard-таблица переходов.
- [BOOK_AUDIT.md, правила 8–14 (раздел «Домен»)](../BOOK_AUDIT.md) — нормативная формулировка: unexported-поля и конструкторы (8), behavior-методы без сеттеров (9), sentinel-ошибки (10), чистота домена (11), закрытые enum (12), stateless-функции (13), бизнес-`if` вне домена = дефект (14).
- [ADR-0003 «Граница агрегата Auction»](../adr/0003-auction-aggregate-boundary.md) — решение «история в границе согласованности, но не в памяти»: контекст, отклонённые альтернативы, последствия.
- [TEXTBOOK.md §4 «Богатая модель против „стены if-ов"»](../TEXTBOOK.md) — конспект-версия этой главы и золотое правило 4: «правило живёт там, где живут данные, которые оно защищает».
- Код: `internal/auction/domain/auction/` (`auction.go`, `money.go`, `bid.go`, `reserve.go`, `schedule.go`, `antisnipe.go`, `status.go`, `listing_rules.go`, `errors.go`), `internal/billing/domain/invoice/` (`invoice.go`, `commission.go`, `errors.go`), маппинг ошибок — `internal/auction/app/command/place_bid.go`.
