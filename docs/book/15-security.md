# Глава 15. Security by design: защита, которую нельзя забыть

## Зачем читать

В 2019 году в Harbor — популярном container registry, который к тому моменту стоял в тысячах инсталляций, — нашли уязвимость CVE-2019-16097. Никакого взлома шифрования, никакого хитрого эксплойта. Один отсутствующий guard: любой зарегистрированный пользователь мог создать администраторский аккаунт через прямой POST к `/api/users`. Просто забытый `if isAdmin`. Разработчик написал новый хендлер, скопировал структуру из соседнего — и граница прав исчезла, не оставив следов ни в тестах, ни на ревью.

Запомните эту историю — мы будем возвращаться к ней всю главу. Потому что большинство security-уязвимостей в серверных приложениях именно такие: не криптография, не социальная инженерия, а пропущенный `if`. И вот неудобная правда: дисциплина и код-ревью эту проблему не решают принципиально. Они снижают вероятность ошибки — но не делают её невозможной. Harbor ревьюили живые, компетентные люди.

Эта глава о другом подходе: когда защита встроена в типы и контракты так, что забыть её применить означает не скомпилировать код. Не «договоримся проверять», а «нельзя не проверить».

---

## Проблема: авторизация как договорённость

Посмотрите на классический паттерн — авторизацию в хендлере. Скорее всего, вы писали такой код; почти все писали:

```go
// Антипаттерн: защита как ручная проверка, которую легко пропустить
func (h *Handler) UpdateAuction(w http.ResponseWriter, r *http.Request) {
    user := getUserFromCtx(r.Context())
    auction := h.repo.GetByID(r.Context(), auctionID)
    if user.ID != auction.SellerID {   // <- этот if можно забыть
        http.Error(w, "forbidden", 403)
        return
    }
    // ... мутация
}
```

На что смотреть: помеченная стрелкой строка — это и есть Harbor в миниатюре. Она ничем не связана с мутацией ниже: удалите её, и код скомпилируется, тесты на happy path пройдут, ревьювер пролистает. Защита держится на том, что каждый следующий автор каждого следующего хендлера вспомнит её написать.

И это не единственная беда здесь. Проверка выполняется до загрузки агрегата в транзакции — то есть над данными, которые к моменту мутации могут устареть (классический TOCTOU). Ответ 403 услужливо сообщает атакующему, что объект существует, — раскрытие информации в подарок. А системный код — воркеры, саги, — у которого «пользователя» нет в принципе, вынужден либо заводить fake-пользователя, либо обходить проверку через специальный флаг. Оба варианта плохи, и оба встречаются в реальных кодовых базах постоянно.

---

## Теория: сделать неправильное невозможным

Secure by design — не набор правил, а сдвиг ответственности: защита должна быть неотъемлемой частью контракта, а не необязательным слоем поверх него. Цель — чтобы ошибку ловил компилятор или тест, а не внимательность ревьювера в пятницу вечером.

Полезная аналогия — турникет на проходной. Табличка «предъявите пропуск» — это `if` в хендлере: работает, пока все помнят и никто не торопится. Турникет — это контракт: через него физически нельзя пройти без карты, и вежливость охранника роли не играет. Всё, что мы построим ниже, — превращение табличек в турникеты.

Принципов пять, и они складываются в систему.

**Явный типизированный acting user.** Если методы репозитория принимают пользователя явным параметром, забыть его передать невозможно — код не скомпилируется. Тип устраняет двусмысленность: `Actor` — это конкретный человек, стоящий за действием, а не «какой-то UUID».

**Правило — чистая доменная функция.** Авторизационное условие живёт в домене, рядом с данными, которые оно защищает. Это обычная функция без сайд-эффектов: легко читать, легко тестировать, невозможно обойти — репозиторий вызывает её принудительно.

**Enforcement внутри транзакции.** Проверка выполняется после `SELECT ... FOR UPDATE` на свежих данных — нет TOCTOU, нет гонки между «прочитали» и «проверили».

**Anti-enumeration по дизайну.** Чужой объект неотличим от несуществующего: ownership — предикат в WHERE, нарушение возвращает NotFoundError наружу, а реальная причина уходит в WARN-лог для аудита.

**Системные операции с именами, которые кричат.** Воркеры и саги не маскируются под пользователей — они вызывают отдельные методы с именем, которое невозможно использовать случайно.

---

## Как в Molot

### Acting user: явный типизированный параметр

Начнём с типа. `Actor` объявлен в домене аукциона (`internal/auction/domain/auction/actor.go`):

```go
// Actor is the acting user behind a user-driven Repository.Update.
// It exists so a mutation's principal is an explicit typed parameter
// (BOOK_AUDIT rule 21) and so system flows are forced through the
// loudly named UpdateAsSystem instead of a fake user (rule 23).
type Actor struct {
    id uuid.UUID
}

func NewActor(id uuid.UUID) (Actor, error) {
    if id == uuid.Nil {
        return Actor{}, ErrInvalidID
    }
    return Actor{id: id}, nil
}

// ActorFromSeller and ActorFromBidder lift typed principals into the
// repository's Actor parameter.
func ActorFromSeller(s SellerID) Actor { return Actor{id: s.UUID()} }
func ActorFromBidder(b BidderID) Actor { return Actor{id: b.UUID()} }
```

На что смотреть: поле `id` приватное — сконструировать `Actor` из произвольного UUID, минуя валидацию, нельзя. Фабрики `ActorFromSeller` / `ActorFromBidder` поднимают уже валидированный доменный идентификатор, а не UUID из воздуха. `Actor` — не псевдоним для `uuid.UUID`, это тип с семантикой: «человек, действующий прямо сейчас».

### Контракт репозитория: компилятор вместо ревьювера

Теперь главный ход. Интерфейс `Repository` в домене (`internal/auction/domain/auction/repository.go`):

```go
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
```

На что смотреть: две сигнатуры закрывают несколько классов ошибок разом. Вызвать `Update` без `actor` — compile error: Harbor-сценарий «забыл проверку» здесь буквально не собирается. Системный поток, по ошибке потянувшийся к `Update` вместо `UpdateAsSystem`, обязан откуда-то взять реального `Actor` — а взять его неоткуда, и первая же попытка вскроется на интеграционном тесте. И имя `UpdateAsSystem` честно кричит о себе в логах: в аудит-трейле сразу видно, что действовал не пользователь, а системный процесс.

Адаптер (`internal/auction/adapters/auction_pg_repository.go`) добавляет ещё один guard:

```go
func (r *AuctionPostgresRepository) Update(
    ctx context.Context,
    id auction.AuctionID,
    actor auction.Actor,
    updateFn func(ctx context.Context, a *auction.Auction) (*auction.Auction, error),
) error {
    if actor.IsZero() {
        return errors.New("update requires an acting user; system flows use UpdateAsSystem")
    }
    return r.update(ctx, id, updateFn)
}
```

На что смотреть: нулевой `Actor` (кто-то передал `auction.Actor{}` напрямую) — явная ошибка в рантайме с текстом, который сразу говорит, что делать. Belt-and-suspenders поверх типовой защиты: типы ловят отсутствие параметра, рантайм-guard — его бессмысленное значение.

### Правило в домене: чистая функция

Само авторизационное условие — не `if` в хендлере, а именованная функция в домене (`internal/auction/domain/auction/auction.go`):

```go
// CanSellerManageAuction is the authorization rule as a pure domain
// function (rule 22).
func CanSellerManageAuction(s SellerID, a Auction) error {
    if s != a.seller {
        return ForbiddenAuctionManagementError{Actor: s, Owner: a.seller}
    }
    return nil
}
```

На что смотреть: функция чистая — никаких сайд-эффектов, никаких зависимостей. Её можно покрыть unit-тестами без единого мока. И она возвращает не безликий `errors.New("forbidden")`, а структуру с контекстом (`Actor`, `Owner`) — готовый материал для аудит-лога.

Вызывается она из доменных методов, которым нужна авторизация:

```go
// Cancel withdraws a listing before the first bid; only the seller may
// do it (authorization is the pure domain rule CanSellerManageAuction).
func (a *Auction) Cancel(by SellerID, now time.Time) error {
    if err := CanSellerManageAuction(by, *a); err != nil {
        return err
    }
    // ... state transition
}
```

На что смотреть: сигнатура `Cancel(by SellerID, ...)` не оставляет лазейки — вызвать отмену, не сказав, *кто* отменяет, синтаксически невозможно. Проверка и мутация склеены в одном методе; use case не может выполнить вторую в обход первой.

Тот же паттерн в billing (`internal/billing/domain/invoice/invoice.go`):

```go
// CanDebtorAccessInvoice is the pure authorization rule (BOOK_AUDIT
// rule 22): only the debtor may see or pay their invoice.
func CanDebtorAccessInvoice(actor BidderID, i Invoice) error {
    if actor != i.debtor {
        return ForbiddenInvoiceAccessError{Actor: actor, Debtor: i.debtor}
    }
    return nil
}
```

На что смотреть: правило повторяет форму, не код — в каждом контексте своя функция со своими типами. Это сознательная цена автономии контекстов, знакомая по дублированию `Money`.

### Enforcement внутри транзакции: нет кода-пути мимо

Чистая функция хороша, но кто гарантирует, что её вызовут? Репозиторий. Адаптер инвойсов (`internal/billing/adapters/invoice_pg_repository.go`) вызывает `CanDebtorAccessInvoice` строго внутри открытой транзакции, после `SELECT ... FOR UPDATE`:

```go
func (r *InvoicePostgresRepository) update(
    ctx context.Context,
    id invoice.InvoiceID,
    actor *invoice.BidderID,
    updateFn func(ctx context.Context, inv *invoice.Invoice) (*invoice.Invoice, error),
) error {
    return postgres.RunInTx(ctx, r.db, func(ctx context.Context, tx *sql.Tx) error {
        row := tx.QueryRowContext(ctx,
            `SELECT `+invoiceColumns+` FROM billing.invoices WHERE id = $1 FOR UPDATE`,
            id.UUID(),
        )
        inv, err := scanInvoice(row)
        // ... error handling

        if actor != nil {
            // Authorization is a pure domain rule executed inside the
            // transaction (BOOK_AUDIT rule 22). Its violation is logged
            // at WARN for the audit trail and leaves the repository as
            // NotFoundError — a foreign invoice must be
            // indistinguishable from a missing one (§2.2).
            if accessErr := invoice.CanDebtorAccessInvoice(*actor, *inv); accessErr != nil {
                r.logger.WarnContext(ctx, "forbidden invoice access",
                    slog.String("context", "billing"),
                    slog.String("invoice_id", id.String()),
                    slog.Any("error", accessErr),
                )
                return invoice.NotFoundError{InvoiceID: id}
            }
        }

        // ... updateFn, persist, publish
    })
}
```

На что смотреть здесь стоит трижды.

Первое: проверка выполняется на только что прочитанном агрегате под локом. Между «проверили» и «изменили» никто не вклинится — TOCTOU закрыт не аккуратностью, а конструкцией.

Второе: нарушение логируется как WARN с реальными деталями (`ForbiddenInvoiceAccessError` несёт и `Actor`, и `Debtor`) — это аудит-трейл для расследования. Но наружу выходит `NotFoundError`: чужой инвойс неотличим от несуществующего. Внутрь — правду, наружу — ничего.

Третье: `actor *invoice.BidderID` — указатель, и `nil` означает системный вызов через `UpdateAsSystem`. Единственный легитимный способ обойти guard — и он явный, читаемый прямо в сигнатуре, а не спрятанный во флаге конфигурации.

> **Нюанс.** Расположение guard-а относительно `updateFn` — не мелочь. Guard до `updateFn` — мутация не началась. Guard после — мутация выполнена, и хотя откат транзакции спасёт данные в базе, он не отменит side effects, если они успели случиться раньше отката (например, событие уже ушло в publish). Правило простое и непреклонное: сначала «можно ли», потом «делаем».

### Anti-enumeration: ownership в WHERE

Для чтения Molot идёт ещё дальше — проверка владения вообще не существует как отдельный шаг. Метод `Get` (`invoice_pg_repository.go`) встраивает ownership прямо в SQL-предикат:

```go
func (r *InvoicePostgresRepository) Get(
    ctx context.Context,
    id invoice.InvoiceID,
    actor invoice.BidderID,
) (*invoice.Invoice, error) {
    // Ownership lives in the WHERE predicate: a foreign invoice is the
    // same NotFoundError as a missing one (anti-enumeration, §2.2).
    row := r.db.QueryRowContext(ctx,
        `SELECT `+invoiceColumns+` FROM billing.invoices WHERE id = $1 AND debtor_id = $2`,
        id.UUID(), actor.UUID(),
    )
    inv, err := scanInvoice(row)
    if errors.Is(err, sql.ErrNoRows) {
        return nil, invoice.NotFoundError{InvoiceID: id}
    }
    // ...
}
```

На что смотреть: `WHERE id = $1 AND debtor_id = $2` совмещает проверку существования и проверку владения в одном запросе. «Нет такого инвойса» и «инвойс чужой» дают одинаковый `sql.ErrNoRows`, одинаковый `NotFoundError`, одинаковый 404 на HTTP-порту. Атакующий, перебирающий UUID-ы, не получает даже самого скромного приза — знания, какие из них существуют.

Аналогичный паттерн в read-side: `InvoiceByID` тоже включает `debtor_id` в WHERE.

### Путь identity: от JWT до команды

Откуда вообще берётся acting user? Здесь действует жёсткий инвариант (BOOK_AUDIT rule 24): identity никогда не путешествует через `context.Context` как произвольное значение между слоями. Разрешённый паттерн ровно один: auth middleware кладёт `User` в контекст по unexported key, порт достаёт его типизированным accessor-ом и немедленно конвертирует в доменный тип.

Middleware (`internal/common/auth/auth.go`):

```go
// ContextWithUser returns ctx carrying user. The middleware calls it on
// every authenticated request; tests use it to fabricate authenticated
// contexts without HTTP.
func ContextWithUser(ctx context.Context, user User) context.Context {
    return context.WithValue(ctx, userCtxKey{}, user)
}

// UserFromCtx returns the authenticated user placed in ctx by the
// middleware. A missing user means the route was wired outside the auth
// middleware — a programming error, surfaced as ErrorKindUnknown (500).
func UserFromCtx(ctx context.Context) (User, error) {
    user, ok := ctx.Value(userCtxKey{}).(User)
    if !ok {
        return User{}, errs.NewUnknownError("no-user-in-context")
    }
    return user, nil
}
```

На что смотреть: ключ — `userCtxKey{}`, unexported struct-тип. Внешний пакет физически не может ни положить, ни прочитать значение по этому ключу. И обратите внимание на поведение при отсутствии пользователя: не «продолжим с нулевым значением», а ошибка уровня 500 — потому что отсутствие пользователя за auth middleware означает ошибку маршрутизации, а не запрос анонима.

HTTP-порт (`internal/auction/ports/http.go`) достаёт `User` и сразу переводит в доменный тип:

```go
func (s HTTPServer) PlaceBid(
    ctx context.Context,
    request PlaceBidRequestObject,
) (PlaceBidResponseObject, error) {
    user, err := auth.UserFromCtx(ctx)
    if err != nil {
        return nil, err
    }
    bidder, err := auction.NewBidderID(user.ID)
    if err != nil {
        return nil, errs.NewIncorrectInputError("invalid-id").WithCause(err)
    }
    // ... bidder передаётся в команду PlaceBid, а не в context
}
```

На что смотреть: дальше по стеку контекст как носитель identity исчезает. Use case получает `BidderID`, репозиторий — `Actor`. Цепочка «кто действует» прослеживается статически, по сигнатурам, — не нужно гадать, что лежит в контексте на третьем слое вглубь.

> **Где вы на это наступите.** Добавляя новый маршрут, вы скопируете регистрацию из соседнего файла — ровно как тот разработчик Harbor. Если при этом маршрут случайно встанет вне auth middleware, `UserFromCtx` вернёт `ErrorKindUnknown`, и первый же интеграционный тест упадёт с 500. Сравните с альтернативой, где accessor «мягко» возвращает пустого пользователя: тест пройдёт, эндпоинт уедет в прод открытым, и узнаете вы об этом из отчёта багхантера. Громкий отказ — это фича.

### JWT-middleware: единая точка, закрытый enum ролей

`NewMiddleware` (`internal/common/auth/auth.go`) — единственный JWT-middleware на весь монолит:

```go
func NewMiddleware(mode, hs256Secret string) (func(http.Handler) http.Handler, error) {
    switch mode {
    case ModeLocalHS256:
        if hs256Secret == "" {
            return nil, errors.New("auth: AUTH_HS256_SECRET must not be empty...")
        }
        return hs256Middleware([]byte(hs256Secret)), nil
    case ModeJWKS:
        return nil, errors.New("auth: AUTH_MODE=jwks is not implemented yet: ...")
    default:
        return nil, fmt.Errorf("auth: unknown AUTH_MODE %q ...", mode, ModeLocalHS256, ModeJWKS)
    }
}
```

На что смотреть: middleware не существует, пока не сконфигурирован корректно. Неверный `AUTH_MODE` или пустой секрет ломают старт процесса — а не первый запрос в три часа ночи. Ошибки конфигурации должны падать на стол того, кто конфигурирует, а не того, кто дежурит.

Роли — закрытый enum (`RoleBidder`, `RoleSeller`, `RoleOperations`); неизвестная роль из токена даёт `401`, а не пустое значение:

```go
role := Role(c.Role)
if !role.valid() {
    return User{}, fmt.Errorf("unknown role claim %q", c.Role)
}
```

На что смотреть: `valid()` — закрытый `switch`. Новая роль требует явного расширения; «как-нибудь обработается само» здесь не вариант по построению.

### Middleware-стек: security headers

`NewRouter` (`internal/common/server/server.go`) фиксирует стек middleware (его подробный разбор по слоям — в главе 16):

```go
// NewRouter builds the root router with the §8 stack
// (RequestID → RealIP → otelhttp → slog request log → Recoverer → CORS
// → security headers) and returns it together with the /api subrouter,
// which additionally applies NoCache and the JWT auth middleware.
func NewRouter(logger *slog.Logger, authMiddleware func(http.Handler) http.Handler) (root *chi.Mux, api chi.Router) {
    r := chi.NewRouter()

    r.Use(middleware.RequestID)
    r.Use(middleware.ClientIPFromRemoteAddr)
    r.Use(otelhttp.NewMiddleware("molot.http"))
    r.Use(spanRouteNamer)
    r.Use(requestLogger(logger))
    r.Use(middleware.Recoverer)
    r.Use(corsMiddleware)
    r.Use(securityHeaders)

    api = r.Route("/api", func(api chi.Router) {
        api.Use(middleware.NoCache)
        api.Use(authMiddleware)
    })

    return r, api
}
```

На что смотреть: auth middleware применяется к суброутеру `/api` целиком — ещё один турникет вместо таблички. Невозможно «забыть навесить auth» на отдельный эндпоинт, потому что auth не навешивается на эндпоинты — он стоит на входе в зону.

`securityHeaders` выставляет минимальный защитный набор:

```go
func securityHeaders(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("X-Content-Type-Options", "nosniff")
        w.Header().Set("X-Frame-Options", "deny")
        next.ServeHTTP(w, r)
    })
}
```

На что смотреть: заголовки выставляются middleware-ом на все ответы, включая ошибочные, — ни один хендлер не обязан о них помнить.

> **Нюанс.** `middleware.ClientIPFromRemoteAddr` стоит здесь вместо привычного `middleware.RealIP` — и это не вкусовщина. `RealIP` признан spoofable (GHSA-3fxj-6jh8-hvhx): он доверяет заголовкам `X-Forwarded-For`, которые контролирует клиент. Его безопасный преемник берёт адрес из `RemoteAddr` — из TCP-соединения, которое подделать нельзя. Если вы за прокси, правильный путь — `ClientIPFromXFFTrustedProxies(n)` с явным числом доверенных прокси. Урок шире самого кейса: «популярный middleware из стандартного набора» — не синоним «безопасный».

`NoCache` на `/api` — отдельная деталь: ответы аутентифицированных эндпоинтов не должны оседать в браузерном кеше.

### Системные операции: имена как документация

Когда closing worker закрывает истёкший аукцион, «пользователя» в человеческом смысле у него нет. Соблазнительный путь — fake-user — антипаттерн: такой UUID ничем не отличается от настоящего, попадает в логи и засоряет аудит мусором, который потом придётся объяснять. Вместо этого системный код вызывает `UpdateAsSystem`, а пользовательский — `Update` с настоящим `Actor`:

```go
// (internal/auction/app/command/cancel_auction.go — аналогичный паттерн)
err := h.repo.Update(ctx, cmd.AuctionID, auction.ActorFromSeller(cmd.Seller),
    func(ctx context.Context, a *auction.Auction) (*auction.Auction, error) {
        if err := a.Cancel(cmd.Seller, h.clock.Now()); err != nil {
            return nil, err
        }
        return a, nil
    })
```

На что смотреть: `ActorFromSeller(cmd.Seller)` читается как утверждение — «это действие совершает продавец». Пользовательские use cases используют `ActorFromSeller` / `ActorFromBidder`, системные — `UpdateAsSystem`. Разница видна в коде, в трейсах, в логах — везде, где вы будете её искать при расследовании.

---

## Трейдоффы

Турникеты не бесплатны — давайте честно посчитаем цену.

**Больше кода для простых случаев.** Три метода вместо одного (`Update`, `UpdateAsSystem`, плюс shared `update`), отдельный тип `Actor`, конверсия в порту — реальный overhead. Для CRUD-контекста (`participant`) он избыточен, и это явно задокументировано: там применён упрощённый вариант без этого слоя. Уровень защиты должен соответствовать ценности защищаемого — бронировать дверь в кладовку незачем.

**Конверсия identity в порту.** `auth.UserFromCtx` + `auction.NewBidderID(user.ID)` — два шага вместо одного. Это намеренное разделение: auth middleware живёт в `common`, доменные типы — в `auction`, и знать друг о друге они не должны. Порт — законное место для маппинга; неуклюжесть здесь дешевле связности.

**404 вместо 403 для forbidden.** С точки зрения security решение корректное, но клиента, ожидающего честного «вы авторизованы, но доступа нет», оно может удивить. Если будущий UX потребует различать случаи, правило anti-enumeration придётся пересматривать — и делать это стоит явным ADR, а не тихой правкой в одном хендлере.

**Один HS256-ключ.** `AUTH_MODE=local-hs256` с единым секретом нормален для разработки и тестирования. Для production не хватает ротации ключей, `kid`-claim, revocation. Это честная граница, отмеченная в коде и расписанная в роадмапе, — о ней ниже.

---

## Типичные ошибки

**Авторизация if-ами по хендлерам.** Каждый хендлер проверяет права сам, и при появлении нового хендлера проверка не переходит автоматически. Это и есть Harbor-паттерн — глава началась с его цены. Диагностический признак виден издалека: авторизационная логика не покрыта доменными тестами, потому что она не в домене.

**403 раскрывает существование.** `if auction.SellerID != user.ID { return 403 }` сообщает атакующему: объект есть, просто не ваш. Перебором ID он составит карту чужих ресурсов. Правильно: 404 наружу, WARN с деталями внутрь.

**Проверка прав после мутации.** `updateFn` вызвана, агрегат изменён, и только потом проверяется владение. Откат транзакции спасёт персистентность — но не побочные эффекты, если guard оказался после `publishEvents`. Правило из врезки выше: guard всегда до `updateFn`, без исключений.

**Bypass-флаги «для тестов».** `if isTest { skipAuth() }`. Назовём вещи своими именами: тест-флаг в продакшн-коде — это не тест-инфраструктура, это дыра с таймером.

> **Совет из практики.** Правильная альтернатива bypass-флагу уже есть в кодовой базе: `GenerateToken` создаёт настоящий валидный JWT с нужными claims, и тест проходит через тот же middleware, что и прод. Это окупается дважды: исчезает дыра — и middleware перестаёт быть слепым пятном, потому что каждый интеграционный тест прогоняет и его. Если в вашем проекте тесты «ходят в обход» auth — это первое, что стоит починить, и это дешевле, чем кажется.

**Identity через `context.Context` между слоями.** Use case сам достаёт `auth.User` из контекста, минуя явный параметр команды. По сигнатуре функции больше не видно, кто действует; контекст можно передать не тот; тест может не покрыть именно этот путь. Контекст — для сквозных служебных вещей (trace, deadline), не для бизнес-данных.

**Fake-пользователи для системных операций.** `SystemUser := User{ID: uuid.Nil, Role: "system"}` — магическое значение, которое ничего не значит в бизнес-домене, но исправно протекает в логи и аудит-трейлы. Отдельный метод `UpdateAsSystem` дешевле и честнее.

---

## Чек-лист

- [ ] Каждый метод репозитория для user-owned агрегата принимает acting user явным типизированным параметром — не `uuid.UUID`, не `context.Context`.
- [ ] Авторизационное правило — именованная чистая функция в домене; покрыта unit-тестами без моков.
- [ ] Guard вызывается внутри транзакции, после `SELECT ... FOR UPDATE`, строго до `updateFn`.
- [ ] Нарушение авторизации логируется как WARN с деталями (кто, к чему, почему отказано).
- [ ] Наружу нарушение идёт как NotFoundError — не как 403.
- [ ] Ownership чужих объектов — предикат в WHERE, а не проверка после загрузки.
- [ ] Системные потоки используют `UpdateAsSystem` или аналог с говорящим именем — никаких fake users.
- [ ] Identity попадает в команду явным полем, не добывается use case'ом из контекста самостоятельно.
- [ ] JWT middleware — один, применяется к `/api` суброутеру целиком, а не хендлер за хендлером.
- [ ] Роли — закрытый enum; неизвестная роль из токена отклоняется с 401.
- [ ] Security headers (`X-Content-Type-Options`, `X-Frame-Options`) выставляются middleware — не в хендлерах.
- [ ] `X-Real-IP` / `X-Forwarded-For` не доверяются без явной конфигурации числа доверенных прокси.
- [ ] Нет тест-флагов, обходящих авторизацию в продакшн-коде.
- [ ] Отсутствие user в контексте — 500, а не тихое продолжение с нулевым значением.

---

## Что здесь сознательно не реализовано (ROADMAP §11)

Молот — обучающий эталон, а не production-grade SaaS, и важная часть зрелости — честно сказать, чего нет. Ряд вещей намеренно отложен, с явным указанием в коде и роадмапе.

**Refresh-токены и revocation (§11.1.2).** Сейчас один долгоживущий HS256 access-токен без revocation. `AUTH_MODE=jwks` отклоняется с actionable ошибкой на старте — это задокументированный future, а не заглушка. Ротация ключей (`kid`-claim), короткий TTL access + refresh с rotation и revocation по logout/компрометации — следующий шаг на пути к production.

**MFA для роли operations (§11.1.3).** Оператор — самый ценный аккаунт для атакующего: кто получил его, получил всё. TOTP + отдельный аудит каждого действия роли `RoleOperations` запланированы, но требуют отдельного auth-эндпоинта и хранения TOTP-секретов.

**TLS (§11.2.1).** Всё сейчас plaintext — нормально для localhost, недопустимо дальше. TLS-терминация на edge (caddy/traefik с auto-cert), `sslmode=require` для Postgres.

**Secrets management (§11.2.2).** `AUTH_HS256_SECRET` и `DATABASE_URL` живут в env-файле. sops/age для репо-секретов, Vault/k8s-secrets в production, reload-порт для ротации без рестарта.

**Лог-гигиена (§11.1.5).** Scrubbing PII и токенов в slog-хендлере — deny-list атрибутов. Сейчас нет гарантий, что токен не попадёт в лог через `%+v` на ошибке.

Порядок из роадмапа продиктован соотношением цены и риска: сначала лог-гигиена и repo-гигиена (дешёвые утечки), затем токены, TLS, secrets и, наконец, MFA ops.

---

## Ссылки

- BOOK_AUDIT.md, правила 21–25: acting user, чистое правило, системные операции, context values, JWT middleware.
- ROADMAP §11: полный backlog AppSec + InfraSec + процесс безопасности с оценками трудоёмкости.
- `internal/auction/domain/auction/actor.go` — тип Actor и фабрики.
- `internal/auction/domain/auction/repository.go` — контракт с явным acting user.
- `internal/auction/adapters/auction_pg_repository.go` — enforcement в адаптере, `SELECT ... FOR UPDATE`.
- `internal/billing/domain/invoice/repository.go` — контракт invoice.Repository с anti-enumeration комментарием.
- `internal/billing/adapters/invoice_pg_repository.go` — ownership в WHERE, WARN-лог, NotFoundError наружу.
- `internal/common/auth/auth.go` — JWT middleware, закрытый enum ролей, GenerateToken для тестов.
- `internal/common/server/server.go` — middleware-стек, security headers, ClientIPFromRemoteAddr.
