# Глава 16. HTTP-контракт: codegen, ошибки, идемпотентность клиента

## Зачем читать

Вспомните, как обычно появляется документация на API: сначала пишутся хендлеры, потом — когда подключается первый внешний потребитель — кто-то садится и описывает «как оно работает» в документе. К концу первого квартала документ и код расходятся, и с этого момента документация не помогает, а вводит в заблуждение: ей нельзя ни верить, ни не верить.

Здесь мы разберём противоположную модель: **спека — источник истины**. Хендлеры обязаны реализовать сгенерированный из спеки интерфейс, и любое несоответствие — это ошибка компиляции, а не сюрприз в production. Бонусом — клиент, который сам генерирует UUID в теле запроса, получает естественную идемпотентность без единой строки дополнительного кода: ретраить можно безбоязненно.

Если провести аналогию: спека-после-кода — это протокол о намерениях, который пишут после сделки. Спека-до-кода — договор, подписанный обеими сторонами, где за нарушение наказывает не юрист, а компилятор.

Конкретный разрез — Molot: аукционная платформа, один бинарь, пять bounded context-ов, chi + oapi-codegen v2 strict server. Всё, что написано ниже, проверено по живому коду; ссылки — реальные файлы репозитория.

---

## Проблема

Типичный эволюционный путь HTTP-слоя в Go-сервисе вы наверняка наблюдали лично. Разработчик пишет хендлер, попутно определяя структуры запроса и ответа прямо в том же файле. Через две недели появляется второй хендлер с похожей логикой ошибок — и она уже другая: где-то `{"error": "not found"}`, где-то `{"message": "auction not found"}`, где-то просто `404` без тела. Документация создаётся постфактум «по коду» — и немедленно начинает отставать. А первый настоящий клиент (мобильное приложение, партнёрский интегратор) обнаруживает все несовместимости разом — в production, в худший из возможных моментов.

Параллельно копятся проблемы структурные, и у каждой есть имя:

- **Anemic endpoints**: `POST /auctions/{id}` с телом `{"field1": ..., "field2": ..., "action": "cancel"}` — один эндпоинт на все действия, маршрутизация по `action` внутри хендлера.
- **Бизнес-валидация в порту**: хендлер проверяет `if req.StartPrice <= 0 { return 400 }`, хотя это инвариант домена — и теперь он живёт в двух местах, которые разойдутся.
- **Статус из use case**: `handle.ErrAuctionClosed` несёт в себе `http.StatusConflict` — приложение знает про HTTP, хотя не должно знать даже о его существовании.
- **200 с ошибкой**: `{"error": "bid too low", "code": 0}` при HTTP 200 — клиент вынужден парсить тело каждого ответа, чтобы понять, успех ли это.

Симптомы разные, корень один: **нет явного контракта**, от которого зависят и сервер, и клиент. Дальше — как его завести.

---

## Теория

### Contract-first: спека — единственный источник истины

Contract-first означает: OpenAPI-спека пишется до кода, а код генерируется из спеки. Ключевое следствие звучит скромно, но меняет всё: **сломать контракт нельзя случайно**. Его можно сломать только осознанно — изменив `.yaml` и перезапустив генератор, то есть совершив действие, которое видно в диффе и обсуждаемо на ревью.

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

На что смотреть: каждый метод принимает и возвращает типизированные Request/Response-объекты, сгенерированные из спеки. Не реализовали метод — программа не компилируется. Вернули тип, которого нет в спеке, — не компилируется. Это и есть «несоответствие = ошибка компиляции» в буквальном смысле.

### Тонкий порт: что должен и чего не должен делать хендлер

Хендлер в Clean Architecture — входящий адаптер, и список его обязанностей короток: распаковать HTTP-запрос в доменные типы, извлечь аутентифицированного пользователя из контекста, собрать команду или запрос, вызвать `app.Handle()` и смапить результат обратно в HTTP-ответ — либо пробросить ошибку.

Чего в этом списке нет — важнее того, что есть. **Никакой бизнес-логики.** Ни одного `if`, принимающего бизнес-решение. Все инварианты — в домене. Все ошибки — slug-ошибки из app-слоя. Хендлер — переводчик на границе, а переводчик не добавляет к сообщению собственных мыслей.

### Slug-ошибки: домен не знает про HTTP

Домен возвращает sentinel-ошибки:

```go
var ErrBidBelowMinimum = errors.New("bid below minimum")
```

На что смотреть: здесь нет ни статуса, ни JSON-формата — только факт на языке домена: «ставка ниже минимума».

App-слой оборачивает их в slug с kind на границе:

```go
// в command handler или app-адаптере
return errs.NewConflictError("bid-below-minimum").WithCause(auction.ErrBidBelowMinimum)
```

На что смотреть: `kind` (Conflict) — это *класс* ошибки, ещё не статус; slug — машиночитаемое имя для клиента; `WithCause` сохраняет исходную ошибку для логов. Три слоя информации, и ни один не про HTTP.

Порт не занимается маппингом вообще — он пробрасывает ошибку, а единый хелпер превращает её в HTTP:

```go
// httperr.RespondWithSlugError — один маппер kind→статус для всего монолита
// IncorrectInput→400, Forbidden→403, NotFound→404, Conflict→409, Unavailable→502, Unknown→500
```

Почему статусы **не из use case**: use case не знает, какой транспорт его вызывает, — и это знание ему незачем. Завтра тот же PlaceBid может быть вызван через gRPC или CLI: slug «bid-below-minimum» переведётся в gRPC `FAILED_PRECONDITION` соседним маппером, а код use case не изменится ни на символ.

### Client-generated UUID и идемпотентность

Теперь — самый недооценённый ход этой главы. Клиент генерирует UUID *до* отправки запроса:

```
POST /api/auctions
{"id": "550e8400-...", "title": "Vintage watch", ...}

→ 204 No Content
Content-Location: /api/auctions/550e8400-...
```

Повторный запрос с тем же `id` возвращает тот же `204` — `ON CONFLICT (id) DO NOTHING` на уровне БД. Это как трек-номер, который отправитель клеит на посылку сам: сколько раз ни приноси её в отделение, второй посылки не возникнет — номер тот же. Клиент может ретраить при сетевых сбоях без риска создать дубликат, и для этого не нужны ни idempotency-заголовки, ни таблицы дедупликации. Аналогично для `PlaceBid`: тот же `bidId` — дубль по первичному ключу в `auction_bids` (append-only таблица), ретрай безопасен.

`Content-Location` указывает, где найти созданный ресурс. Команды бизнес-данных не возвращают — `204`, не `201` с телом. Это решение с дальним прицелом: если завтра команда станет асинхронной, контракт не изменится — клиент и так не ждёт тела.

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

На что смотреть: четыре контекста — четыре пары «спека + конфиг», каждая генерирует свой пакет `ports`. Изменение контракта — это ровно три шага: правка `.yaml`, `make openapi`, компиляция.

`openapi.gen.go` никогда не редактируется вручную — это зафиксировано в BOOK_AUDIT (правило 55) и в заголовке самого файла.

> **Где вы на это наступите.** Однажды — в спешке, перед демо — вы (или коллега) добавите поле прямо в `.gen.go`, потому что «так быстрее, потом перенесу в спеку». Код соберётся, демо пройдёт. А через неделю кто-то другой запустит `make openapi` по совершенно другому поводу — и генератор молча затрёт правку. Тесты упадут, и хорошо ещё, если тесты. Урок дешевле выучить заранее: generated-файл — не место для ручных правок, даже «на минутку».

> **Совет из практики.** Закрепите это правило в CI: шаг, который запускает `make openapi` и падает при непустом `git diff`. Это копеечная проверка, и она закрывает сразу два сценария — ручные правки в `.gen.go` и изменённую спеку, для которой забыли перегенерировать код. Контракт остаётся источником истины не потому, что все хорошие, а потому что иначе сборка красная.

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

На что смотреть: slug-значения перечислены прямо в `description` каждого статуса — клиентский разработчик видит полный словарь ошибок эндпоинта, не заглядывая в серверный код. `Error` — единственный тип ошибки во всём API: `{"slug": "bid-below-minimum"}`. Одна форма, выученная один раз.

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

На что смотреть: порядок строк здесь — не алфавитный и не случайный; каждый слой стоит там, где стоит, по причине:

- **RequestID** — первым, чтобы все следующие слои видели request-id в логах.
- **ClientIPFromRemoteAddr** — на месте deprecated `RealIP`; записывает peer-адрес без доверия клиентским заголовкам (подробности этой замены — в главе 15). За доверенным прокси — `ClientIPFromXFFTrustedProxies`.
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

Полезное упражнение: возьмите любые два соседних слоя и спросите, что сломается, если их поменять местами. Логгер до otelhttp — пропали `trace_id` из request-логов; Recoverer выше requestLogger — паника не попадёт в лог запроса. Стек читается как зависимости, а не как перечень.

### Полный хендлер PlaceBid

Теперь посмотрим, как выглядит «тонкий порт» из теории, когда он написан целиком. `internal/auction/ports/http.go`:

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

На что смотреть: хендлер длинный, но монотонный — это конвейер из однотипных шагов, и в этой монотонности его достоинство.

Первый шаг — **`auth.UserFromCtx(ctx)`**, типизированный accessor из `internal/common/auth`. Если пользователя в контексте нет, это программная ошибка (маршрут подключён мимо auth middleware), и она честно превращается в `ErrorKindUnknown` → 500. JWT в хендлере не парсится — это уже сделал middleware.

Дальше — цепочка конструкторов доменных типов: `NewBidderID`, `NewAuctionID`, `NewBidID`, `NewCurrency`, `NewMoney`. Каждый возвращает валидированный тип или ошибку, которую хендлер оборачивает в `errs.NewIncorrectInputError` с конкретным slug. Заметьте: это единственная «if-подобная» логика в порту, и она про *формат*, не про *смысл* — граница, которую легко удерживать.

Команда `command.PlaceBid` несёт `auction.BidID`, `auction.BidderID`, `auction.Money` — не `string` и не `int64`. Перепутать поля местами не даст компилятор.

Вызов `s.app.Commands.PlaceBid.Handle(ctx, ...)` — единственная встреча с бизнес-логикой, и хендлер не знает, что происходит внутри. Нарушен инвариант — домен вернёт sentinel, app-слой обернёт в slug, хендлер пробросит наверх.

И наконец, `PlaceBid204Response` — типизированный ответ из `.gen.go`. Вернуть 200 вместо 204 не получится — тип не совпадёт. `Content-Location` несёт путь к созданной ставке.

Итог, который стоит проговорить: ни одного бизнес-`if`. Ни одного `http.StatusXxx` руками. Никакой маршрутизации по полю из тела.

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

На что смотреть: `statusFromKind` — вся таблица маппинга целиком, 5 бизнес-классов ошибок → 5 HTTP-статусов, в одном switch-е, который читается за десять секунд. Логирование разделено по уровням: клиентские ошибки — WARN (это шум, но полезный), серверные — ERROR (это уже к дежурному). И присмотритесь к телу ответа: наружу уходит только `{"slug": "..."}`. Причина (`WithCause`) остаётся в логах — без стектрейсов, внутренних сообщений и деталей инфраструктуры в публичном API.

### Антиenumeration: 404 и для чужого, и для несуществующего

Один тонкий пример из `api/openapi/auction.yaml` (billing-контекст):

```
GET /api/invoices/{invoiceID}
→ 200 (только должник)
→ 404 invoice-not-found — и для несуществующего, и для чужого
```

`InvoiceByID` — ownership в `WHERE id=$1 AND debtor_id=$2`. Чужой инвойс неотличим от несуществующего: оба дают `sql.ErrNoRows` → `NotFoundError` → 404. С точки зрения чистоты семантики HTTP это «неправда» — но неправда осознанная: 403 сообщил бы атакующему «инвойс существует, просто не ваш», и перебор ID превратился бы в разведку. Реальный `ForbiddenInvoiceAccessError{Actor, Debtor}` логируется на WARN — аудит-трейл сохраняется, наружу не протекает ничего. Это HTTP-проекция правила anti-enumeration из главы 15.

### 502 для PSP: контракт с клиентом на уровне статуса

`POST /api/invoices/{invoiceID}/payment`:

```
204 — оплата принята (включая идемпотентный повтор уже оплаченного)
409 invoice-no-longer-payable — инвойс просрочен или аннулирован
409 payment-declined — PSP отклонил карту
502 psp-unavailable — PSP недоступен, попробуйте позже
```

На что смотреть: четыре исхода — четыре разных инструкции клиенту. Два 409 различаются slug-ом, потому что реакция разная: просроченный инвойс ретраить бессмысленно, отклонённую карту — менять. А 502 — это явное «повтори позже»: `ErrorKindUnavailable` → 502 через `statusFromKind`, и клиент, умеющий ретраить на 502, получает автоматическое восстановление после сетевых сбоев к PSP.

> **Нюанс.** Почему 502, а не 503? 503 означает «сервис перегружен или на обслуживании» — наш сервис. 502 — «я шлюз, и проблема за моей спиной». PSP недоступен — это вторая ситуация: наша проблема, не клиента, и статус честно это сообщает. Выбор кажется педантизмом, пока вы не настроите алерты: rate 503 говорит «мы падаем», rate 502 — «падает партнёр». Смешайте их — и потеряете различие, которое в инциденте дороже всего.

### Версионирование: события V1 уже, HTTP — дорога

Интеграционные события Molot уже версионированы: `AuctionListedV1`, `BidPlacedV1`, `AuctionClosedV1` — суффикс в имени типа и в имени структуры. Схемы append-only: изменение поля = новый тип `V2` + dual-publish на период миграции.

HTTP API в текущей итерации версии не несёт — `/api/auctions`, не `/api/v1/auctions`. Несимметрия не случайна: у событий уже сегодня несколько независимых потребителей в разных контекстах, у HTTP — один (первая версия UI). Версионирование HTTP (path, header, media type) описано в ARCHITECTURE §8 как roadmap 8.7 — отложенное решение за существующими интерфейсами портов. Отложенное — не значит забытое: значит, зафиксированное там, где его найдут, когда придёт время.

---

## Трейдоффы

### Contract-first vs code-first

Contract-first требует дисциплины — но обратите внимание, где именно. Изменить код вразрез со спекой невозможно (компилятор не даст), а вот изменить спеку, не подумав о последствиях, — легко: генератор исполнит любую вашу ошибку с одинаковым энтузиазмом. Контракт защищает от рассинхрона, не от плохих решений. Ценность подхода растёт с числом потребителей API; для внутреннего API одной команды code-first с ретроспективной спекой может быть разумным компромиссом — если честно принять, что документация будет отставать.

### 204 vs 201

`204 No Content` + `Content-Location` против `201 Created` с телом ресурса. Молот выбирает 204: команды не возвращают бизнес-данных (CQRS). Нужны данные созданного аукциона — клиент идёт по `Content-Location` и делает GET. Да, это лишний round-trip, и для чувствительного к латентности UI он может быть заметен. Но взамен сервер сохраняет право сделать команду асинхронной без смены контракта — `201` с телом закрепил бы синхронность навсегда. Это плата за свободу манёвра, и она названа явно.

### Client-generated UUID vs server-generated

Клиент генерирует UUID — клиент и отвечает за уникальность. Звучит тревожно, но арифметика успокаивает: вероятность коллизии UUID v4 — порядка 1/2^61 в год, это приемлемо. Выигрыш: идемпотентность из коробки, без отдельных idempotency-ключей в заголовках и таблиц дедупликации. Проигрыш: клиент должен уметь генерировать UUID — для браузеров это `crypto.randomUUID()`, для мобильных — платформенные API. На практике порог давно не проблема, но в контракте он есть, и о нём стоит сказать интегратору заранее.

### Один маппер slug→статус vs декларативный в спеке

`statusFromKind` — 5-строчный switch в одном файле. Альтернатива — прописывать статус в каждом `SlugError` — выглядит гибче, но размазывает транспортную политику по бизнес-коду. Молот выбирает первое: kind→status — это политика транспортного слоя, не бизнес-логики. Проверка выбора — расширяемость: понадобится gRPC — рядом появится `kindToGRPCCode`, и ни домен, ни app-слой не узнают об этом.

---

## Типичные ошибки

### Anemic endpoints

Один эндпоинт `PATCH /auctions/{id}` с телом `{"action": "cancel", "data": {...}}`, а в хендлере — `switch req.Action { case "cancel": ...; case "relist": ... }`. Это RPC, переодетый в REST-урл, и расплата приходит по всем фронтам сразу: в спеке у такого эндпоинта нечего описать, в метриках все действия слипаются в одно, права доступа приходится различать внутри switch-а. Каждое действие — отдельный эндпоинт с семантически значимым именем: `POST /auctions/{id}/cancellation`, `POST /auctions/{id}/bids`. Молот именно так и устроен: пять `POST`-эндпоинтов для разных действий над аукционом.

### 200 с ошибкой в теле

`HTTP 200` + `{"success": false, "error": "bid too low"}`. Клиент обязан парсить тело каждого ответа, чтобы узнать, успех ли это. Хуже того — мониторинг слеп: на дашборде сплошные 200, error rate нулевой, а пользователи не могут сделать ставку. `statusFromKind` решает это структурно: ошибка = non-2xx статус + `{"slug": "..."}`, единая форма на всём API.

### Бизнес-валидация в порту

```go
// Антипаттерн
if req.Body.StartPriceMinor <= 0 {
    return nil, errs.NewIncorrectInputError("negative-price")
}
```

На что смотреть: проверка выглядит безобидно и даже заботливо — но `NewMoney` уже делает её внутри. Теперь правило живёт в двух местах, и однажды они разойдутся: в домене порог поменяют, в порту забудут. Порт не дублирует бизнес-правила — он вызывает конструкторы доменных типов и оборачивает результат в slug. Лакмус: **если проверка защищает инвариант — её место в домене**.

### Разные форматы ошибок по эндпоинтам

`/api/auctions/{id}` возвращает `{"error": "..."}`, `/api/invoices/{id}` — `{"message": "..."}`, `/api/participants` — просто строку. Так получается само собой, когда каждый хендлер пишет собственный `json.Marshal`. Единственный `RespondWithSlugError` закрывает проблему на уровне архитектуры — его нельзя «не вызвать», потому что хендлеры вообще не пишут ответ напрямую: они возвращают типизированный объект или ошибку, остальное — забота инфраструктуры.

### HTTP-статус из use case

```go
// Антипаттерн в app-слое
type ErrAuctionClosed struct { StatusCode int }
```

На что смотреть: одно поле `StatusCode` — и app-слой уже знает про HTTP, а значит, привязан к транспорту, которого по своей роли не должен видеть. Use case знает о *конфликте*; что конфликт — это 409, знает только транспортный слой. Slug + kind — единственное, что пересекает границу app→ports, а статус рождается в `statusFromKind`: одно место, один раз.

---

## Чек-лист

- [ ] Спека `.yaml` написана до хендлера; хендлер генерируется, не пишется вручную.
- [ ] `.gen.go` зафиксирован в `.gitignore` или явно помечен как generated — `make openapi` воспроизводимо.
- [ ] Все хендлеры реализуют `StrictServerInterface` — несоответствие контракту ловит компилятор, а не клиент.
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
