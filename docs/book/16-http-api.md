# Глава 16. HTTP-контракт: codegen, ошибки, идемпотентность клиента

## Зачем читать

Большинство проектов начинают с обратного: сначала пишут хендлеры, потом описывают API в документе — и к концу первого квартала документ расходится с кодом. Здесь мы разберём другую модель: **спека — источник истины**, хендлеры обязаны реализовать сгенерированный интерфейс (несоответствие — ошибка компиляции), а клиент с UUID в теле запроса получает естественную идемпотентность без единой строки дополнительного кода.

Конкретный разрез — Molot: аукционная платформа, один бинарь, пять bounded context-ов, chi + oapi-codegen v2 strict server. Всё что написано ниже проверено по живому коду, ссылки — реальные файлы репозитория.

---

## Проблема

Типичный эволюционный путь HTTP-слоя в Go-сервисе выглядит так:

1. Разработчик пишет хендлер, попутно определяя структуру запроса и ответа прямо в том же файле.
2. Через две недели появляется новый хендлер с похожей логикой ошибок — и она уже другая: где-то `{"error": "not found"}`, где-то `{"message": "auction not found"}`, где-то просто `404` без тела.
3. Документация создаётся постфактум «по коду» — и немедленно начинает отставать.
4. Первый клиент (мобильное приложение, партнёрский интегратор) обнаруживает несовместимые форматы только в production.

Параллельно накапливаются структурные проблемы:

- **Anemic endpoints**: `POST /auctions/{id}` с телом `{"field1": ..., "field2": ..., "action": "cancel"}` — один эндпоинт на все действия, логика маршрутизации по `action` внутри хендлера.
- **Бизнес-валидация в порту**: хендлер проверяет `if req.StartPrice <= 0 { return 400 }`, хотя это инвариант домена — он не живёт в одном месте.
- **Статус из use case**: `handle.ErrAuctionClosed` несёт в себе `http.StatusConflict` — приложение знает про HTTP.
- **200 с ошибкой**: `{"error": "bid too low", "code": 0}` при HTTP 200 — клиентам приходится парсить тело для определения успеха.

Все эти проблемы имеют общий корень: **нет явного контракта**, от которого зависит и сервер, и клиент.

---

## Теория

### Contract-first: спека — единственный источник истины

Contract-first означает, что OpenAPI-спека пишется до кода, а код генерируется из спеки. Ключевое следствие: **сломать контракт нельзя случайно** — его можно сломать только осознанно, изменив `.yaml` и перезапустив генератор.

oapi-codegen в режиме `strict-server` генерирует не просто структуры запросов/ответов, но и **интерфейс сервера**:

```go
// Сгенерировано из auction.yaml — никогда не редактируется вручную
type StrictServerInterface interface {
    ListAuction(ctx context.Context, request ListAuctionRequestObject) (ListAuctionResponseObject, error)
    PlaceBid(ctx context.Context, request PlaceBidRequestObject) (PlaceBidResponseObject, error)
    AuctionCard(ctx context.Context, request AuctionCardRequestObject) (AuctionCardResponseObject, error)
    // ...
}
```

Если хендлер не реализует метод — программа не компилируется. Если метод возвращает тип, не соответствующий спеке — программа не компилируется. Это и есть «несоответствие = ошибка компиляции».

### Тонкий порт: что должен и чего не должен делать хендлер

Хендлер в Clean Architecture — это входящий адаптер. Его единственные обязанности:

1. Распаковать HTTP-запрос в доменные типы.
2. Извлечь аутентифицированного пользователя из контекста.
3. Собрать команду / запрос с доменными типами.
4. Вызвать `app.Handle()`.
5. Смапить результат обратно в HTTP-ответ или пробросить ошибку.

**Никакой бизнес-логики.** Ни одного `if`, принимающего бизнес-решение. Все инварианты — в домене. Все ошибки — slug-ошибки из app-слоя.

### Slug-ошибки: домен не знает про HTTP

Домен возвращает sentinel-ошибки:

```go
var ErrBidBelowMinimum = errors.New("bid below minimum")
```

App-слой оборачивает их в slug с kind на границе:

```go
// в command handler или app-адаптере
return errs.NewConflictError("bid-below-minimum").WithCause(auction.ErrBidBelowMinimum)
```

Порт не занимается маппингом вообще — он пробрасывает ошибку, а единый хелпер превращает её в HTTP:

```go
// httperr.RespondWithSlugError — один маппер kind→статус для всего монолита
// IncorrectInput→400, Forbidden→403, NotFound→404, Conflict→409, Unavailable→502, Unknown→500
```

Почему статусы **не из use case**: use case не знает, какой транспорт его вызывает. Завтра тот же хендлер PlaceBid может быть вызван через gRPC или CLI — slug «bid-below-minimum» переведётся в gRPC `FAILED_PRECONDITION` тем же механизмом, а код use case не изменится.

### Client-generated UUID и идемпотентность

Клиент генерирует UUID до отправки запроса:

```
POST /api/auctions
{"id": "550e8400-...", "title": "Vintage watch", ...}

→ 204 No Content
Content-Location: /api/auctions/550e8400-...
```

Повторный запрос с тем же `id` возвращает тот же `204` — `ON CONFLICT (id) DO NOTHING` на уровне БД. Клиент может ретраить при сетевых сбоях без риска создать дубликат. Аналогично для `PlaceBid`: тот же `bidId` — дубль по первичному ключу в `auction_bids` (append-only таблица), ретрай безопасен.

`Content-Location` указывает, где найти созданный ресурс. Команды бизнес-данных не возвращают — `204`, не `201` с телом. Если завтра `ListAuction` станет асинхронной командой — контракт не меняется: клиент уже не ждёт тела.

---

## Как в Molot

### Конфигурация генератора

`api/openapi/cfg-auction.yaml`:

```yaml
package: ports
generate:
  chi-server: true
  strict-server: true
  models: true
  embedded-spec: true
output: internal/auction/ports/openapi.gen.go
```

`Makefile`, цель `openapi`:

```makefile
openapi:
	go tool oapi-codegen -config api/openapi/cfg-auction.yaml api/openapi/auction.yaml
	go tool oapi-codegen -config api/openapi/cfg-participant.yaml api/openapi/participant.yaml
	go tool oapi-codegen -config api/openapi/cfg-billing.yaml api/openapi/billing.yaml
	go tool oapi-codegen -config api/openapi/cfg-settlement.yaml api/openapi/settlement.yaml
```

`openapi.gen.go` никогда не редактируется вручную — это зафиксировано в BOOK_AUDIT (правило 55) и в заголовке файла. Изменение контракта = изменение `.yaml` + `make openapi` + компиляция. Попытка вручную добавить метод в `.gen.go` будет затёрта при следующем запуске генератора.

### Спека как документация: ошибки прямо в YAML

`api/openapi/auction.yaml`, эндпоинт `PlaceBid`:

```yaml
post:
  operationId: PlaceBid
  summary: Place a bid (bid id is client-generated)
  responses:
    "204":
      description: Bid accepted
      headers:
        Content-Location:
          schema:
            type: string
    "403":
      description: seller-cannot-bid | verification-required
      content:
        application/json:
          schema:
            $ref: "#/components/schemas/Error"
    "409":
      description: bid-below-minimum | leader-cannot-outbid-self | auction-not-open
      content:
        application/json:
          schema:
            $ref: "#/components/schemas/Error"
```

Slug-значения прописаны в `description` — спека одновременно документирует контракт для клиентов и служит ориентиром для реализации. `Error` — единственный тип ошибки во всём API: `{"slug": "bid-below-minimum"}`.

### Middleware-стек: порядок и смысл каждого слоя

`internal/common/server/server.go`:

```go
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

Порядок не случаен:

- **RequestID** — первым, чтобы все следующие слои видели request-id в логах.
- **ClientIPFromRemoteAddr** — на месте deprecated `RealIP`; записывает peer-адрес без доверия клиентским заголовкам. За доверенным прокси — `ClientIPFromXFFTrustedProxies`.
- **otelhttp** — до логгера, чтобы логгер мог прочитать span из контекста и записать `trace_id`.
- **spanRouteNamer** — после роутинга chi (через `defer` с `chi.RouteContext`), переименовывает span с шаблоном пути вместо общего имени:
  ```go
  func spanRouteNamer(next http.Handler) http.Handler {
      return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          defer func() {
              if pattern := chi.RouteContext(r.Context()).RoutePattern(); pattern != "" {
                  trace.SpanFromContext(r.Context()).SetName(r.Method + " " + pattern)
              }
          }()
          next.ServeHTTP(w, r)
      })
  }
  ```
- **requestLogger** — после otelhttp, пишет один slog-запись на запрос с методом, путём, статусом, длительностью и request-id.
- **Recoverer** — ловит паники в хендлерах, превращает в 500.
- **CORS и securityHeaders** (`X-Content-Type-Options: nosniff`, `X-Frame-Options: deny`) — применяются ко всем ответам, включая ошибочные.
- **NoCache и authMiddleware** — только для `/api`, отдельный subrouter.

### Полный хендлер PlaceBid

`internal/auction/ports/http.go`:

```go
func (s HTTPServer) PlaceBid(ctx context.Context, request PlaceBidRequestObject) (PlaceBidResponseObject, error) {
    user, err := auth.UserFromCtx(ctx)
    if err != nil {
        return nil, err
    }
    bidder, err := auction.NewBidderID(user.ID)
    if err != nil {
        return nil, errs.NewIncorrectInputError("invalid-id").WithCause(err)
    }
    auctionID, err := auction.NewAuctionID(request.AuctionID)
    if err != nil {
        return nil, errs.NewIncorrectInputError("invalid-id").WithCause(err)
    }
    bidID, err := auction.NewBidID(request.Body.BidId)
    if err != nil {
        return nil, errs.NewIncorrectInputError("invalid-bid").WithCause(err)
    }
    currency, err := auction.NewCurrency(request.Body.Currency)
    if err != nil {
        return nil, errs.NewIncorrectInputError("currency-mismatch").WithCause(err)
    }
    amount, err := auction.NewMoney(request.Body.AmountMinor, currency)
    if err != nil {
        return nil, errs.NewIncorrectInputError("invalid-bid").WithCause(err)
    }

    if err := s.app.Commands.PlaceBid.Handle(ctx, command.PlaceBid{
        AuctionID: auctionID,
        BidID:     bidID,
        Bidder:    bidder,
        Amount:    amount,
    }); err != nil {
        return nil, err
    }

    location := "/api/auctions/" + auctionID.String() + "/bids/" + bidID.String()
    return PlaceBid204Response{
        Headers: PlaceBid204ResponseHeaders{ContentLocation: &location},
    }, nil
}
```

Разберём структуру:

1. **`auth.UserFromCtx(ctx)`** — типизированный accessor из `internal/common/auth`. Если пользователя нет в контексте — программная ошибка (маршрут подключён не туда), возвращает `ErrorKindUnknown` → 500. JWT не парсится в хендлере — middleware сделал это раньше.

2. **Конструкторы доменных типов** — каждый `NewBidderID`, `NewAuctionID`, `NewBidID`, `NewCurrency`, `NewMoney` возвращает валидированный доменный тип или ошибку. Хендлер оборачивает её в `errs.NewIncorrectInputError` с конкретным slug — это единственная «бизнес-if-подобная» логика в порту, и она — про формат, не про смысл.

3. **Команда с доменными типами** — `command.PlaceBid` несёт `auction.BidID`, `auction.BidderID`, `auction.Money`, не `string`/`int64`. Компилятор гарантирует, что перепутать поля невозможно.

4. **`s.app.Commands.PlaceBid.Handle(ctx, ...)`** — единственный вызов бизнес-логики. Хендлер не знает, что происходит внутри. Если `PlaceBid` нарушает инвариант — домен вернёт sentinel, app-слой обернёт в slug, хендлер пробросит ошибку.

5. **`PlaceBid204Response`** — типизированный ответ из `.gen.go`. Нельзя вернуть 200 вместо 204 — тип не совпадёт. `Content-Location` несёт путь к созданной ставке.

Ни одного бизнес-`if`. Ни одного `http.StatusXxx` вручную. Никакой логики маршрутизации по полю из тела.

### Единый маппер ошибок

`internal/common/server/httperr/httperr.go`:

```go
func RespondWithSlugError(err error, w http.ResponseWriter, r *http.Request) {
    if err == nil {
        err = errors.New("httperr: RespondWithSlugError called with nil error")
    }

    slug := internalSlug
    var slugErr errs.SlugError
    if errors.As(err, &slugErr) && slugErr.Slug != "" {
        slug = slugErr.Slug
    }

    status := statusFromKind(errs.KindFromError(err))

    logger := slog.Default().With(
        slog.Any("error", err),
        slog.String("slug", slug),
        slog.Int("status", status),
    )
    if status >= http.StatusInternalServerError {
        logger.ErrorContext(r.Context(), "request failed with server error")
    } else {
        logger.WarnContext(r.Context(), "request failed with client error")
    }

    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    w.WriteHeader(status)
    _ = json.NewEncoder(w).Encode(map[string]string{"slug": slug})
}

func statusFromKind(kind errs.ErrorKind) int {
    switch kind {
    case errs.ErrorKindIncorrectInput:
        return http.StatusBadRequest
    case errs.ErrorKindForbidden:
        return http.StatusForbidden
    case errs.ErrorKindNotFound:
        return http.StatusNotFound
    case errs.ErrorKindConflict:
        return http.StatusConflict
    case errs.ErrorKindUnavailable:
        return http.StatusBadGateway
    default:
        return http.StatusInternalServerError
    }
}
```

Здесь видна вся таблица маппинга: 5 бизнес-классов ошибок → 5 HTTP-статусов. `Unavailable` → 502 (не 503): PSP недоступен — это наша проблема, не клиента; клиент должен ретраить. Логирование разделяется: клиентские ошибки — WARN, серверные — ERROR.

Причина ошибки (`WithCause`) остаётся в логах. Наружу уходит только slug — без стектрейсов, внутренних сообщений, деталей инфраструктуры.

### Антиenumeration: 404 и для чужого, и для несуществующего

Один тонкий пример из `api/openapi/auction.yaml` (billing-контекст):

```
GET /api/invoices/{invoiceID}
→ 200 (только должник)
→ 404 invoice-not-found — и для несуществующего, и для чужого
```

`InvoiceByID` — ownership в `WHERE id=$1 AND debtor_id=$2`. Чужой инвойс неотличим от несуществующего: оба дают `sql.ErrNoRows` → `NotFoundError` → 404. Это осознанная утечка информации: 403 сообщил бы «инвойс существует, но не ваш». Реальный `ForbiddenInvoiceAccessError{Actor, Debtor}` логируется на WARN — аудит-трейл сохраняется, ничего не протекает клиенту.

### 502 для PSP: контракт с клиентом на уровне статуса

`POST /api/invoices/{invoiceID}/payment`:

```
204 — оплата принята (включая идемпотентный повтор уже оплаченного)
409 invoice-no-longer-payable — инвойс просрочен или аннулирован
409 payment-declined — PSP отклонил карту
502 psp-unavailable — PSP недоступен, попробуйте позже
```

502 для PSP — не случайность. Это явный сигнал клиенту: «повтори запрос». `ErrorKindUnavailable` → 502 через `statusFromKind`. Клиент, умеющий ретраить на 502, получает автоматическое восстановление после сетевых сбоев к PSP.

### Версионирование: события V1 уже, HTTP — дорога

Интеграционные события Molot уже версионированы: `AuctionListedV1`, `BidPlacedV1`, `AuctionClosedV1` — суффикс в имени типа и в имени структуры. Схемы append-only: изменение поля = новый тип `V2` + dual-publish на период миграции.

HTTP API в текущей итерации версии не несёт — `/api/auctions`, не `/api/v1/auctions`. Это сознательное решение: MVP-масштаб, один потребитель (первая версия UI). Версионирование HTTP (path, header, media type) описано в ARCHITECTURE §8 как roadmap 8.7 — отложенное решение за существующими интерфейсами портов.

---

## Трейдоффы

### Contract-first vs code-first

Contract-first требует дисциплины: изменение в коде без изменения спеки невозможно (компилятор), но изменение спеки без понимания последствий — легко. Преимущество обратно пропорционально размеру команды: чем больше потребителей API, тем ценнее явный контракт. Для внутреннего API одной команды код-first с ретроспективной спекой может быть разумным компромиссом — если принять, что документация будет отставать.

### 204 vs 201

`204 No Content` + `Content-Location` vs `201 Created` с телом создаваемого ресурса. Молот выбирает 204: команды не возвращают бизнес-данных (CQRS). Если клиенту нужны данные созданного аукциона — он следует `Content-Location` и делает GET. Это дополнительный round-trip, но сохраняет право сделать команду асинхронной без смены контракта. `201` с телом закрепил бы синхронность навсегда.

### Client-generated UUID vs server-generated

Клиент генерирует UUID → клиент несёт ответственность за уникальность. На практике UUID v4 с вероятностью коллизии ~1/2^61 в год — приемлемо. Выигрыш: идемпотентность из коробки, нет необходимости в отдельных idempotency-ключах в заголовках. Проигрыш: клиент должен уметь генерировать UUID — для браузеров это `crypto.randomUUID()`, для мобильных — платформенные API.

### Один маппер slug→статус vs декларативный в спеке

`statusFromKind` — 5-строчный switch в одном файле. Альтернатива: прописывать статус в каждом `SlugError`. Молот выбирает первое: kind-→-status — это политика транспортного слоя, не бизнес-логики. Если завтра нужно добавить gRPC — добавляется `kindToGRPCCode` рядом, без касания ни домена, ни app-слоя.

---

## Типичные ошибки

### Anemic endpoints

Один эндпоинт `PATCH /auctions/{id}` с телом `{"action": "cancel", "data": {...}}`. В хендлере: `switch req.Action { case "cancel": ...; case "relist": ... }`. Это замаскированный RPC под REST-урлом. Каждое действие — отдельный эндпоинт с семантически значимым именем: `POST /auctions/{id}/cancellation`, `POST /auctions/{id}/bids`. Молот именно так: пять `POST`-эндпоинтов для разных действий над аукционом.

### 200 с ошибкой в теле

`HTTP 200` + `{"success": false, "error": "bid too low"}`. Клиент обязан парсить тело на каждый запрос, чтобы понять успех. Мониторинг не видит ошибок (все запросы 200). `statusFromKind` решает это структурно: ошибка = non-2xx статус + `{"slug": "..."}`. Единая форма на всём API.

### Бизнес-валидация в порту

```go
// Антипаттерн
if req.Body.StartPriceMinor <= 0 {
    return nil, errs.NewIncorrectInputError("negative-price")
}
```

`NewMoney` уже проверяет это внутри и возвращает ошибку. Порт не дублирует бизнес-правила — он только вызывает конструкторы доменных типов и оборачивает результат в slug. Правило: **если проверка нужна для защиты инварианта — она в домене**.

### Разные форматы ошибок по эндпоинтам

`/api/auctions/{id}` возвращает `{"error": "..."}`, `/api/invoices/{id}` — `{"message": "..."}`, `/api/participants` — просто строку. Это происходит, когда каждый хендлер пишет собственный `json.Marshal`. Единственный `RespondWithSlugError` решает проблему на уровне архитектуры — его просто нельзя не вызвать, потому что хендлеры не пишут ответ напрямую: они возвращают типизированный объект или ошибку.

### HTTP-статус из use case

```go
// Антипаттерн в app-слое
type ErrAuctionClosed struct { StatusCode int }
```

Use case знает о конфликте, но не знает, что для HTTP это 409. Slug + kind — единственное что пересекает границу app→ports. Статус определяется в `statusFromKind` — одно место, один раз.

---

## Чек-лист

- [ ] Спека `.yaml` написана до хендлера; хендлер генерируется, не пишется вручную.
- [ ] `.gen.go` зафиксирован в `.gitignore` или явно помечен как generated — `make openapi` воспроизводимо.
- [ ] Все хендлеры реализуют `StrictServerInterface` — ошибка компиляции при несоответствии.
- [ ] Каждый create-endpoint принимает client-generated UUID, отвечает 204 + `Content-Location`.
- [ ] Ни один хендлер не содержит бизнес-`if` — только маппинг типов и проброс ошибки.
- [ ] `auth.UserFromCtx` — единственный способ получить пользователя; JWT не парсится в хендлере.
- [ ] Все ошибки через `httperr.RespondWithSlugError` — единый формат `{"slug": "..."}` на всём API.
- [ ] `statusFromKind` — один switch; статусы не прописаны ни в домене, ни в use case.
- [ ] `Unavailable` → 502, не 503; документировано в спеке в `description` эндпоинта.
- [ ] Middleware-стек зафиксирован в одном месте (`NewRouter`); порядок задокументирован.
- [ ] Анти-enumeration для ownership-ресурсов: `WHERE id=$1 AND owner_id=$2` → 404 для чужого.
- [ ] Ошибки от PSP/внешних систем не протекают клиенту — только slug, причина в логах.
- [ ] Slug-значения прописаны в `description` спеки — документация = единственный источник контракта.

---

## Ссылки

- ARCHITECTURE §8 — HTTP API: middleware-стек, таблица эндпоинтов, правило 31 (204 + Content-Location), slug-ошибки (правило 30).
- BOOK_AUDIT пп. 30–31 — CQRS: ошибки ports-agnostic, create-эндпоинты с UUID на стороне клиента.
- BOOK_AUDIT п. 55 — контракты через codegen, `.gen.go` руками не правится.
- `api/openapi/auction.yaml` — спека auction-контекста.
- `api/openapi/cfg-auction.yaml` — конфигурация oapi-codegen.
- `internal/auction/ports/http.go` — все хендлеры auction-контекста.
- `internal/common/server/httperr/httperr.go` — `RespondWithSlugError`, `statusFromKind`.
- `internal/common/server/server.go` — `NewRouter`, middleware-стек.
- `internal/common/errs/errors.go` — `SlugError`, `ErrorKind`.
- `internal/common/auth/auth.go` — `UserFromCtx`, `NewMiddleware`, `GenerateToken`.
- `Makefile`, цель `openapi` — команда генерации.
