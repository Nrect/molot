# Глава 15. Security by design: защита, которую нельзя забыть

## Зачем читать

Большинство security-уязвимостей в серверных приложениях — не взлом шифрования и не социальная инженерия. Это пропущенный `if`. Разработчик написал новый хендлер, скопировал структуру из соседнего, забыл вызвать `authCheck()` — и граница прав исчезла. Именно так появился CVE-2019-16097 в Harbor: один отсутствующий guard позволял любому зарегистрированному пользователю создать администраторский аккаунт через прямой POST к `/api/users`. Никаких хитростей — просто забытый `if isAdmin`.

Дисциплина и код-ревью не решают эту проблему принципиально: они снижают вероятность ошибки, но не делают её невозможной. Эта глава о другом подходе — когда защита встроена в типы и контракты так, что забыть её применить означает не скомпилировать код.

---

## Проблема: авторизация как договорённость

Классический паттерн — авторизация в хендлере:

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

Проблем несколько. Во-первых, каждый хендлер — отдельная точка отказа: добавился новый эндпоинт, проверка могла не перейти вместе с ним. Во-вторых, проверка выполняется до загрузки агрегата в транзакции, то есть над данными, которые к моменту мутации могут устареть (TOCTOU). В-третьих, 403 сообщает атакующему, что объект существует, — это раскрытие информации. В-четвёртых, системный код (workers, sagas) либо требует fake-пользователя, либо обходит проверку через специальный флаг — и оба варианта плохи.

---

## Теория: сделать неправильное невозможным

Secure by design — не набор правил, а архитектурное решение: защита должна быть неотъемлемой частью контракта, а не необязательным слоем поверх него. Цель — сделать ошибку детектируемой компилятором или тестами, а не ревьювером.

Ключевые принципы:

**Явный типизированный acting user.** Если методы репозитория принимают пользователя явным параметром, забыть его передать невозможно — код не скомпилируется. Тип устраняет двусмысленность: `Actor` — это конкретный человек, стоящий за действием.

**Правило — чистая доменная функция.** Авторизационное условие живёт в домене, рядом с данными, которые оно защищает. Это обычная функция без сайд-эффектов — легко читать, легко тестировать, невозможно обойти: репозиторий вызывает её принудительно.

**Enforcement внутри транзакции.** Проверка выполняется после `SELECT ... FOR UPDATE` на свежих данных — нет TOCTOU, нет состояния гонки между «прочитали» и «проверили».

**Anti-enumeration по дизайну.** Чужой объект неотличим от несуществующего: ownership — предикат в WHERE, нарушение возвращает NotFoundError наружу, реальная причина — в WARN-логе для аудита.

**Системные операции с именами, которые кричат.** Воркеры и саги не создают fake-пользователей, а вызывают отдельные методы с явным именем, которое невозможно случайно использовать.

---

## Как в Molot

### Acting user: явный типизированный параметр

Тип `Actor` объявлен в домене аукциона (`internal/auction/domain/auction/actor.go`):

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

`Actor` — не просто `uuid.UUID`. Это тип с семантикой: «человек, действующий прямо сейчас». Приватное поле делает конструирование контролируемым. Фабрики `ActorFromSeller` / `ActorFromBidder` поднимают уже валидированный доменный идентификатор — не UUID из воздуха.

### Контракт репозитория: компилятор вместо ревьювера

Интерфейс `Repository` в домене (`internal/auction/domain/auction/repository.go`):

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

Эти две сигнатуры делают невозможным несколько классов ошибок одновременно. `Update` без `actor` — compile error. Системный поток, случайно вызвавший `Update` вместо `UpdateAsSystem`, обязан передать реального `Actor` — получить его неоткуда, ошибка обнаружится при интеграционном тесте. Имя `UpdateAsSystem` в логах сразу видно в аудит-трейле: это не обычный пользователь, это системный процесс.

Адаптер (`internal/auction/adapters/auction_pg_repository.go`) добавляет дополнительный guard:

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

Нулевой `Actor` — явная ошибка в рантайме. Belt-and-suspenders поверх типовой защиты.

### Правило в домене: чистая функция

Авторизационная логика — не `if` в хендлере, а именованная функция в домене (`internal/auction/domain/auction/auction.go`):

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

Функция принимает конкретные типы, возвращает именованную структуру ошибки с контекстом (`Actor`, `Owner`) — для аудит-лога. Она вызывается из доменных методов, которые требуют авторизации:

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

Вызывающий (use case) не может провести мутацию в обход проверки: `Cancel` без `SellerID` просто не имеет смысла как вызов.

Аналогичный паттерн в billing (`internal/billing/domain/invoice/invoice.go`):

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

### Enforcement внутри транзакции: нет кода-пути мимо

Репозиторий инвойсов (`internal/billing/adapters/invoice_pg_repository.go`) вызывает `CanDebtorAccessInvoice` строго внутри открытой транзакции, после `SELECT ... FOR UPDATE`:

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

Несколько деталей заслуживают внимания.

Во-первых, проверка выполняется на только что прочитанном агрегате под локом — никакого TOCTOU между «проверили» и «изменили».

Во-вторых, нарушение логируется как WARN с реальными деталями (`ForbiddenInvoiceAccessError` содержит и `Actor`, и `Debtor`) — это аудит-трейл для расследования. Но наружу выходит `NotFoundError` — чужой инвойс неотличим от несуществующего.

В-третьих, `actor *invoice.BidderID` — указатель. `nil` означает системный вызов (`UpdateAsSystem`), проверка пропускается. Это единственная легитимная причина обойти guard — и она явная в сигнатуре.

### Anti-enumeration: ownership в WHERE

Чтение инвойса (`invoice_pg_repository.go`, метод `Get`) хранит ownership прямо в SQL-предикате:

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

`WHERE id = $1 AND debtor_id = $2` — одним запросом совмещается проверка существования и проверка владения. Оба случая — «нет такого инвойса» и «инвойс чужой» — возвращают одинаковый `sql.ErrNoRows`, который маппится в одинаковый `NotFoundError`. HTTP-порт отдаст 404 в обоих случаях. Атакующий, перебирающий UUIDs, не узнает, какие из них реально существуют.

Аналогичный паттерн в read-side: `InvoiceByID` тоже включает `debtor_id` в WHERE.

### Путь identity: от JWT до команды

Identity никогда не путешествует через `context.Context` как произвольное значение между слоями — это один из ключевых инвариантов (BOOK_AUDIT rule 24). Разрешённый паттерн: auth middleware кладёт `User` в контекст по unexported key, порт достаёт его типизированным accessor'ом и немедленно конвертирует в доменный тип для команды.

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

Key — `userCtxKey{}`, unexported struct-тип. Никакой внешний пакет не может положить или прочитать значение по этому ключу напрямую.

HTTP-порт (`internal/auction/ports/http.go`) достаёт `User` и немедленно конвертирует в доменный тип:

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

Дальше по стеку — только доменные типы. Use case получает `BidderID`, не `context.Context`. Репозиторий получает `Actor`, не `context.Context`. Цепочка прослеживается статически.

Если маршрут зарегистрирован вне auth middleware, `UserFromCtx` вернёт ошибку `ErrorKindUnknown` (500), а не тихо продолжит с нулевым пользователем. Этот сигнал обнаруживается в первом же интеграционном тесте.

### JWT-middleware: единая точка, закрытый enum ролей

`NewMiddleware` (`internal/common/auth/auth.go`) реализует единственный JWT-middleware для всего монолита:

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

Middleware не жив до тех пор, пока не сконфигурирован корректно — неверный `AUTH_MODE` или пустой секрет ломают старт процесса, а не первый запрос. Роли — закрытый enum (`RoleBidder`, `RoleSeller`, `RoleOperations`); неизвестная роль из токена даёт `401`, а не пустое значение:

```go
role := Role(c.Role)
if !role.valid() {
    return User{}, fmt.Errorf("unknown role claim %q", c.Role)
}
```

`valid()` реализован как закрытый `switch` — добавление новой роли требует явного расширения, нет возможности случайно пропустить обработку.

### Middleware-стек: security headers

`NewRouter` (`internal/common/server/server.go`) описывает стек middleware:

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

Отдельного внимания заслуживает `middleware.ClientIPFromRemoteAddr` вместо устаревшего `middleware.RealIP`. Комментарий в коде явно объясняет причину: `RealIP` признан spoofable (GHSA-3fxj-6jh8-hvhx) — он доверяет заголовкам `X-Forwarded-For` от клиента. Его безопасный преемник записывает адрес из `RemoteAddr`, не из заголовков, которые клиент может подделать. За прокси — правильный способ: `ClientIPFromXFFTrustedProxies(n)` с явным числом доверенных прокси.

`NoCache` на `/api` — отдельная деталь: браузер не должен кешировать ответы аутентифицированных эндпоинтов.

### Системные операции: имена как документация

Когда closing worker закрывает истёкший аукцион, он не имеет «пользователя» в человеческом смысле. Fake-user — антипаттерн: такой UUID ничем не отличается от реального, попадает в логи, создаёт шум в аудите. Вместо этого use case явно вызывает `UpdateAsSystem`:

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

Пользовательские use cases используют `ActorFromSeller` / `ActorFromBidder`. Системные — `UpdateAsSystem`. Разница видна в коде, видна в трейсах, видна в логах.

---

## Трейдоффы

**Больше кода для простых случаев.** Три метода вместо одного (`Update`, `UpdateAsSystem`, плюс shared `update`), отдельный тип `Actor`, конверсия в порту — это реальный overhead. Для CRUD-контекста (`participant`) он избыточен, что явно задокументировано: там применён упрощённый вариант без этого слоя.

**Конверсия identity в порту.** `auth.UserFromCtx` + `auction.NewBidderID(user.ID)` — два шага вместо одного. Это намеренное разделение: auth middleware живёт в `common`, доменные типы — в `auction`. Они не должны знать друг о друге; порт — законное место для маппинга.

**404 вместо 403 для forbidden.** Это корректное решение с точки зрения security, но может удивить клиента, ожидающего явного «вы авторизованы, но у вас нет доступа». Если будущий UX потребует различать случаи, правило anti-enumeration придётся пересмотреть — желательно с явным ADR.

**Один HS256-ключ.** `AUTH_MODE=local-hs256` с единым секретом нормален для разработки и тестирования. Для production отсутствует ротация ключей, нет `kid`-claim, нет revocation. Это честная граница, отмеченная в коде и расписанная в роадмапе.

---

## Типичные ошибки

**Авторизация if-ами по хендлерам.** Каждый хендлер проверяет права сам. При появлении нового хендлера проверка не переходит автоматически — Harbor-паттерн. Признак: авторизационная логика не покрыта доменными тестами.

**403 раскрывает существование.** `if auction.SellerID != user.ID { return 403 }` сообщает, что объект существует, но пользователь не его владелец. Атакующий узнаёт о существовании ресурса через перебор ID. Правильно: 404 наружу, WARN внутрь.

**Проверка прав после мутации.** `updateFn` вызвана, агрегат изменён, только потом проверяется владение. Мутация раскатилась — откат транзакции поможет с персистентностью, но не с побочными эффектами (уже опубликованными событиями, если guard расположен после `publishEvents`). Правило: guard всегда до `updateFn`.

**Bypass-флаги «для тестов».** `if isTest { skipAuth() }`. Тест-флаги в продакшн-коде — это не тест-инфраструктура, это дыра. Правильная альтернатива: `GenerateToken` создаёт настоящий валидный JWT с нужными claims, тест проходит через тот же middleware, что и прод.

**Identity через `context.Context` между слоями.** Use case достаёт `auth.User` из контекста сам, минуя явный параметр команды. Это нарушает прозрачность потока данных: по сигнатуре функции невидно, кто действует. Дополнительно: контекст можно передать неправильный, тест может не проверить именно этот путь.

**Fake-пользователи для системных операций.** `SystemUser := User{ID: uuid.Nil, Role: "system"}` — магическое значение, которое не имеет смысла в бизнес-домене, но протекает в логи и аудит-трейлы. Отдельный метод `UpdateAsSystem` дешевле и честнее.

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

Молот — обучающий эталон, а не production-grade SaaS. Ряд вещей намеренно отложен с честным указанием в коде и роадмапе:

**Refresh-токены и revocation (§11.1.2).** Сейчас один долгоживущий HS256 access-токен без revocation. `AUTH_MODE=jwks` отклоняется с actionable ошибкой на старте — это задокументированный future. Ротация ключей (`kid`-claim), короткий TTL access + refresh с rotation и revocation по logout/компрометации — это следующий шаг при движении к production.

**MFA для роли operations (§11.1.3).** Оператор — самый ценный аккаунт для атакующего. TOTP + отдельный аудит каждого действия роли `RoleOperations` запланированы, но требуют отдельного auth-эндпоинта и хранения TOTP-секретов.

**TLS (§11.2.1).** Всё сейчас plaintext — нормально для localhost, недопустимо дальше. TLS-терминация на edge (caddy/traefik с auto-cert), `sslmode=require` для Postgres.

**Secrets management (§11.2.2).** `AUTH_HS256_SECRET` и `DATABASE_URL` живут в env-файле. sops/age для репо-секретов, Vault/k8s-secrets в production, reload-порт для ротации без рестарта.

**Лог-гигиена (§11.1.5).** Scrubbing PII и токенов в slog-хендлере — deny-list атрибутов. Сейчас нет гарантий, что токен не попадёт в лог через `%+v` на ошибке.

Порядок из роадмапа: лог-гигиена и repo-гигиена (дешёвые утечки) → токены → TLS → secrets → MFA ops.

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
