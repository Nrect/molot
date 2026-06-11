# internal/common — инфраструктурный фундамент

Контракт для разработчиков контекстов: какие пакеты есть, какие публичные API стабильны.
`internal/common` содержит **ноль бизнес-типов** (BOOK_AUDIT правило 4); общие бизнес-понятия (`Money` и т.п.) дублируются по контекстам.

> ✅ **Статус: фундамент доставлен полностью.** Все пакеты реализованы, зависимости запинены в
> go.mod (см. пин-сет в конце). `go build` / `go vet` / `go test -race` / `golangci-lint run` — зелёные.
> API ниже — фактический, проверен тестами.

---

## `molot/internal/common/errs`

Transport-agnostic slug-ошибки. App-слой оборачивает доменные sentinel-ы в `SlugError` на своей границе; ports переводят в HTTP одним хелпером (`httperr.RespondWithSlugError`).

```go
type ErrorKind struct{ /* закрытый enum */ }
var ErrorKindUnknown, ErrorKindIncorrectInput, ErrorKindForbidden,
    ErrorKindNotFound, ErrorKindConflict, ErrorKindUnavailable ErrorKind
func (k ErrorKind) String() string
func (k ErrorKind) IsZero() bool

type SlugError struct {
    Slug string      // стабильный клиентский идентификатор, напр. "bid-below-minimum"
    Kind ErrorKind
    // unexported cause
}
func (e SlugError) Error() string                    // "slug" либо "slug: cause"
func (e SlugError) Unwrap() error                    // errors.Is/As видят доменный sentinel сквозь SlugError
func (e SlugError) Is(target error) bool             // матч по Slug+Kind, cause игнорируется
func (e SlugError) WithCause(cause error) SlugError  // копия с обёрнутой причиной

func NewIncorrectInputError(slug string) SlugError   // → 400
func NewForbiddenError(slug string) SlugError        // → 403
func NewNotFoundError(slug string) SlugError         // → 404
func NewConflictError(slug string) SlugError         // → 409
func NewUnavailableError(slug string) SlugError      // → 502
func NewUnknownError(slug string) SlugError          // → 500
func KindFromError(err error) ErrorKind              // первый SlugError в цепочке; иначе Unknown
```

Паттерн использования в command handler:
`return errs.NewConflictError("already-closed").WithCause(auction.ErrAlreadyClosed)` —
порт получит 409 + `{"slug":"already-closed"}`, лог декоратора — полную причину.

## `molot/internal/common/config`

Загрузка env с fail-fast: **одна ошибка перечисляет все** отсутствующие/невалидные переменные разом.

```go
type AuthMode string   // AuthModeLocalHS256 ("local-hs256") | AuthModeJWKS ("jwks", будущее)
type PSPMode string    // PSPModeSuccess | PSPModeDecline | PSPModeFlaky
type LogFormat string  // LogFormatJSON | LogFormatText

type Config struct {
    PlatformCurrency         string        // PLATFORM_CURRENCY (required, "EUR")
    CommissionBasisPoints    int           // COMMISSION_BASIS_POINTS (default 1000, 0..10000)
    VerifyAboveMinor         int64         // VERIFY_ABOVE_MINOR (default 100000, ≥0)
    SnipeWindow              time.Duration // SNIPE_WINDOW (default 5m)
    SnipeExtension           time.Duration // SNIPE_EXTENSION (default 5m)
    SnipeMaxExtensions       int           // SNIPE_MAX_EXTENSIONS (default 3, ≥0)
    PaymentTerm              time.Duration // PAYMENT_TERM (default 48h)
    PSPMode                  PSPMode       // PSP_MODE (default success)
    RelistDelay              time.Duration // RELIST_DELAY (default 1h)
    RelistDuration           time.Duration // RELIST_DURATION (default 24h)
    ClosingPollInterval      time.Duration // CLOSING_POLL_INTERVAL (default 1s)
    ExpiryPollInterval       time.Duration // EXPIRY_POLL_INTERVAL (default 5s)
    HTTPPort                 int           // HTTP_PORT (default 8080)
    DatabaseURL              string        // DATABASE_URL (required)
    OTELExporterOTLPEndpoint string        // OTEL_EXPORTER_OTLP_ENDPOINT (default localhost:4317)
    AuthMode                 AuthMode      // AUTH_MODE (default local-hs256)
    AuthHS256Secret          string        // AUTH_HS256_SECRET (required при local-hs256)
    LogFormat                LogFormat     // LOG_FORMAT (default json)
}

func Load() (Config, error)
```

Валидация: валюта — 3 заглавные буквы; duration > 0; порт 1–65535; enum-ы закрыты.
`AUTH_MODE=jwks` валиден для Load, но `auth.NewMiddleware` отклоняет его с понятным текстом — старт монолита падает.

## `molot/internal/common/logs`

```go
func NewLogger(format string) *slog.Logger
    // "text" → TextHandler, иначе JSON; обёрнут контекст-хендлером

func NewContextHandler(next slog.Handler) slog.Handler
    // тот же продакшен-обогатитель для кастомных sink-ов/тестов

func ContextWithCorrelationID(ctx context.Context, correlationID string) context.Context
func CorrelationIDFromContext(ctx context.Context) (string, bool)
```

Каждая запись обогащается из контекста: `correlation_id` (кладёт consumer-сторона watermill,
см. observe-middleware) и `trace_id`/`span_id` из OTel span context — автоматически, если в ctx есть активный спан.

## `molot/internal/common/decorator`

Generic cross-cutting на каждый command/query handler (логирование, RED-метрики, спаны). Один `*Decorators` на контекст-модуль.

```go
type CommandHandler[C any] interface { Handle(ctx context.Context, cmd C) error }
type QueryHandler[Q any, R any] interface { Handle(ctx context.Context, query Q) (R, error) }

func NewDecorators(contextName string, logger *slog.Logger,
    meterProvider metric.MeterProvider, tracerProvider trace.TracerProvider) (*Decorators, error)

func ApplyCommandDecorators[C any](handler CommandHandler[C], d *Decorators) CommandHandler[C]
func ApplyQueryDecorators[Q, R any](handler QueryHandler[Q, R], d *Decorators) QueryHandler[Q, R]
```

- Имя use case — из имени типа команды/запроса рефлексией (указатели разыменовываются): `command.PlaceBid` → `PlaceBid`.
- Спан `commands/<Name>` / `queries/<Name>` (tracing — внешний слой, логи и метрики видят span context).
- Лог defer-ом на named err: `command handler succeeded|failed` + атрибуты `context`, `handler`, `error`.
- Гистограммы `molot_command_duration_seconds` / `molot_query_duration_seconds` (unit `s`) с атрибутами `context`/`handler`/`result(ok|err)`.

## `molot/internal/common/metrics`, `molot/internal/common/tracing`

OTLP gRPC init (insecure — локальный collector из docker-compose), resource `service.name=molot`.

```go
metrics.NewMeterProvider(ctx, endpoint string) (*sdkmetric.MeterProvider, error)
    // + Go runtime instrumentation; ставит global otel.SetMeterProvider
tracing.NewTracerProvider(ctx, endpoint string) (*sdktrace.TracerProvider, error)
    // + W3C propagator (TraceContext+Baggage); ставит global otel.SetTracerProvider
```

Оба провайдера возвращаются вызывающему; graceful `Shutdown(ctx)` — обязанность main (см. `shutdownGracefully`).

## `molot/internal/common/postgres`

```go
func NewDB(ctx context.Context, dsn string) (*sql.DB, error)
    // pgx stdlib драйвер + otelsql (спаны запросов, sql.DBStats метрики пула); пингует с ctx

func RunInTx(ctx context.Context, db *sql.DB, fn func(ctx context.Context, tx *sql.Tx) error) error
    // named err + defer FinishTransaction — идиома BOOK_AUDIT §4 п.18

func FinishTransaction(err error, tx *sql.Tx) error
    // экспортирован для адаптеров с собственным BeginTx;
    // rollback при ошибке, упавший rollback — multierr.Combine (оригинал не теряется)
```

## `molot/internal/common/auth`

JWT chi-middleware (ARCHITECTURE §8). HS256 локально; `jwks` — задокументированное будущее (конструктор возвращает ошибку → старт падает).

```go
const ModeLocalHS256 = "local-hs256"; ModeJWKS = "jwks"  // = значениям config.AuthMode

type Role string  // RoleBidder | RoleSeller | RoleOperations
type User struct { ID uuid.UUID; Role Role }

func NewMiddleware(mode, hs256Secret string) (func(http.Handler) http.Handler, error)
func ContextWithUser(ctx context.Context, user User) context.Context  // для тестов портов
func UserFromCtx(ctx context.Context) (User, error)   // нет юзера → errs Unknown ("no-user-in-context")
func GenerateToken(secret string, user User, ttl time.Duration) (string, error)
    // dev/тесты; тот же claims-код-путь, что и валидация; отрицательный ttl → просроченный токен
```

Claims: `sub` (uuid), `role`, `exp` (обязателен), `iat`. Невалидный/отсутствующий токен → `401 {"slug":"unauthorized"}` (та же JSON-форма, что httperr).

## `molot/internal/common/server` (+ `server/httperr`)

```go
func NewRouter(logger *slog.Logger, authMiddleware func(http.Handler) http.Handler) (root *chi.Mux, api chi.Router)
    // root: RequestID → ClientIPFromRemoteAddr → otelhttp → span-route-namer →
    //       slog request log → Recoverer → CORS → security headers (nosniff, X-Frame-Options: deny)
    // api = root.Route("/api"): + NoCache → JWT-auth; контексты монтируют RegisterHTTP сюда

type ReadinessProbe struct { Name string; Check func(ctx context.Context) error }
func RegisterHealthEndpoints(r chi.Router, logger *slog.Logger, readiness []ReadinessProbe)
    // GET /healthz (liveness, всегда 200); GET /readyz (503 на первой упавшей пробе)

func RunHTTPServer(ctx context.Context, addr string, handler http.Handler) error
    // graceful shutdown по ctx, 10s drain

httperr.RespondWithSlugError(err error, w http.ResponseWriter, r *http.Request)
    // ErrorKind → статус: IncorrectInput 400, Forbidden 403, NotFound 404,
    // Conflict 409, Unavailable 502, Unknown/прочее 500; тело {"slug":"..."}
    // не-SlugError → 500 {"slug":"internal-server-error"}; полная ошибка — только в лог
```

## `molot/internal/common/watermill`

```go
const DeadLetterTopic = "events.dead_letter"
var Marshaler = cqrs.JSONMarshaler{GenerateName: cqrs.StructName}  // единый для bus и processor

func NewLogger(logger *slog.Logger) watermill.LoggerAdapter

func NewRouter(logger watermill.LoggerAdapter, deadLetterPublisher message.Publisher, meterProvider metric.MeterProvider) (*message.Router, error)
    // middleware СТРОГО: CorrelationID → PoisonQueue(deadLetter) →
    // Retry{5, exp backoff 100ms×2 cap 30s} → retryCounter →
    // Recoverer → observe
    // retryCounter: при ошибке хендлера инкрементирует
    //   molot_bus_retries_total{handler} — каждая ошибка = один retry-триггер
    // observe: extract W3C trace из metadata → span "events/<HandlerName>",
    //          correlation_id → ctx (логи хендлера получают его автоматически)

func NewSQLSubscriber(db *sql.DB, consumerGroup string, logger watermill.LoggerAdapter) (message.Subscriber, error)
    // consumer group = имя хендлера (независимый offset); InitializeSchema: true
func NewSQLPublisher(db *sql.DB, logger watermill.LoggerAdapter) (message.Publisher, error)
    // по пулу, AutoInitializeSchema: true; для инфраструктуры (dead letter)
func NewTxPublisher(tx *sql.Tx, logger watermill.LoggerAdapter) (message.Publisher, error)
    // outbox: publish в открытой *sql.Tx; схему НЕ инициализирует (implicit commit)

func NewTracingPublisherDecorator(pub message.Publisher) message.Publisher
    // producer-спан "publish <topic>" + inject W3C traceparent в metadata
    // (NewSQLPublisher/NewTxPublisher уже обёрнуты)

func NewEventBus(publisher message.Publisher,
    generateTopic func(eventName string) string, logger watermill.LoggerAdapter) (*cqrs.EventBus, error)
func NewEventProcessor(router *message.Router,
    generateTopic func(eventName string) string,
    subscriberConstructor func(handlerName string) (message.Subscriber, error),
    logger watermill.LoggerAdapter) (*cqrs.EventProcessor, error)
    // AckOnUnknownEvent: true — топики мульти-событийные, хендлер потребляет один тип;
    // чужие имена событий на топике ack-аются молча, не уходят по ретраям в dead letter

func RegisterBusMetrics(db *sql.DB, meterProvider metric.MeterProvider) error
    // observable-гейджи §11 по watermill-таблицам (скан на каждом metric collection):
    // molot_bus_dead_letter_size — не доставленные заново сообщения dead-letter топика;
    // molot_bus_oldest_message_age_seconds{topic} — возраст старейшего сообщения,
    // не ack-нутого самой медленной consumer group топика.
    // Счётчик molot_bus_retries_total{handler} регистрируется в NewRouter.
```

`generateTopic` мапит имя события (`AuctionClosedV1`) на топик — контексты публикуют свои `events.Topic` константы.

## `molot/internal/<ctx>/events` (auction, billing, participant)

Не common, но фиксируем здесь как часть фундамента: единственные пакеты контекстов, импортируемые снаружи. Только плоские V1-структуры с json-тегами + константа топика:

```go
auctionevents.Topic     = "auction-events"     // 8 событий §4.2 ARCHITECTURE.md
billingevents.Topic     = "billing-events"     // 3 события
participantevents.Topic = "participant-events" // 2 события
```

Время — `time.Time` UTC. Схемы append-only: изменение = V2 + dual-publish.

---

## Отклонения от изначального плана (зафиксированы как контракт)

| Было в плане | Стало | Почему |
|---|---|---|
| `postgres.NewPool` | `postgres.NewDB(ctx, dsn)` | по финальному ТЗ интеграции; пингует внутри |
| `ApplyCommandDecorators[C](h, logger, metricsClient, tracer)` | `NewDecorators(contextName, …)` + `Apply*(h, d)` | контекст-модуль задаётся один раз; generic-методы в Go невозможны |
| `auth.GenerateFakeJWT` | `auth.GenerateToken` | тот же код-путь, имя без "fake" — это полноценный HS256-эмитент для local-mode |
| `server.New` | `server.NewRouter → (root, api)` | контекстам нужен доступ к /api-группе ПОСЛЕ применения её middleware |
| `server.RespondWithSlugError` | подпакет `server/httperr` | ports импортируют httperr, не тянут весь server |
| `middleware.RealIP` (§8) | `middleware.ClientIPFromRemoteAddr` | RealIP deprecated в chi 5.3 как спуфабельный (GHSA-3fxj-6jh8-hvhx); за доверенным прокси → `ClientIPFromXFFTrustedProxies(n)` |
| `watermill.PublishInTx` | `watermill.NewTxPublisher(tx, logger)` | реальный API watermill-sql v4: publisher конструируется над `Tx` |
| readiness `[]func(ctx) error` | `[]ReadinessProbe{Name, Check}` | имя пробы нужно в логе и теле 503 |

## Пин-сет (фактический, в go.mod)

```
github.com/ThreeDotsLabs/watermill v1.5.2
github.com/ThreeDotsLabs/watermill-sql/v4 v4.1.5
github.com/go-chi/chi/v5 v5.3.0
github.com/jackc/pgx/v5 v5.10.0
github.com/pressly/goose/v3 v3.27.1
github.com/oapi-codegen/runtime v1.4.1 (+ oapi-codegen/v2 v2.7.1 как tool)
go.opentelemetry.io/otel v1.44.0 (+ metric, sdk, sdk/metric, trace v1.44.0)
go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.44.0
go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.44.0
go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0
go.opentelemetry.io/contrib/instrumentation/runtime v0.69.0
github.com/XSAM/otelsql v0.42.0
github.com/golang-jwt/jwt/v5 v5.3.1
github.com/google/uuid v1.6.0
go.uber.org/multierr v1.11.0
github.com/stretchr/testify v1.11.1
golang.org/x/sync v0.20.0
```
