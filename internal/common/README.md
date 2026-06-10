# internal/common — инфраструктурный фундамент

Контракт для контекст-агентов: какие пакеты есть, какие публичные API стабильны.
`internal/common` содержит **ноль бизнес-типов** (BOOK_AUDIT правило 4); общие бизнес-понятия (`Money` и т.п.) дублируются по контекстам.

> ⚠️ **Статус: фундамент доставлен частично.** Коммит a5f939c «pin dependencies» фактически пуст —
> `go mod tidy` выкинул все require (код их ещё не импортировал), в go.mod остался только tool-chain
> oapi-codegen. Правка go.mod этому агенту запрещена («не хватает депа → deviations, не добавляй»).
> Реализовано всё, что собирается на stdlib; остальные пакеты **отсутствуют в дереве** — их API
> зафиксирован ниже как план, чтобы контекст-агенты не гадали. Восстановленный пин-сет — в конце файла.

---

## ✅ Реализовано (можно импортировать, API стабилен)

### `molot/internal/common/errs`

Transport-agnostic slug-ошибки. App-слой оборачивает доменные sentinel-ы в `SlugError` на своей границе; ports переводят в HTTP одним хелпером (`server.RespondWithSlugError`, появится позже).

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

### `molot/internal/common/config`

Загрузка env с fail-fast: **одна ошибка перечисляет все** отсутствующие/невалидные переменные разом.

```go
type AuthMode string   // AuthModeLocalHS256 ("local-hs256") | AuthModeJWKS ("jwks", будущее)
type PSPMode string    // PSPModeSuccess | PSPModeDecline | PSPModeFlaky
type LogFormat string  // LogFormatJSON | LogFormatText

type Config struct {
    PlatformCurrency         string        // PLATFORM_CURRENCY (required, "EUR")
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
`AUTH_MODE=jwks` валиден для Load, но старт монолита падает с понятным текстом (см. cmd/monolith/main.go; проверка переедет в `auth.NewMiddleware`).

### `molot/internal/common/logs`

```go
func NewLogger(format string) *slog.Logger
    // "text" → TextHandler, иначе JSON; обёрнут контекст-хендлером

func NewContextHandler(next slog.Handler) slog.Handler
    // тот же продакшен-обогатитель для кастомных sink-ов/тестов

func ContextWithCorrelationID(ctx context.Context, correlationID string) context.Context
func CorrelationIDFromContext(ctx context.Context) (string, bool)
```

Каждая запись с контекстом, в котором есть correlation id, получает атрибут `correlation_id`.
**Deviation:** атрибуты `trace_id`/`span_id` из OTel span context — часть контракта этого хендлера,
но требуют otel-зависимость; добавляются внутрь `contextHandler.Handle` без смены публичных сигнатур.

### `molot/internal/<ctx>/events` (auction, billing, participant)

Не common, но фиксируем здесь как часть фундамента: единственные пакеты контекстов, импортируемые снаружи. Только плоские V1-структуры с json-тегами + константа топика:

```go
auctionevents.Topic     = "auction-events"     // 8 событий §4.2 ARCHITECTURE.md
billingevents.Topic     = "billing-events"     // 3 события
participantevents.Topic = "participant-events" // 2 события
```

Время — `time.Time` UTC. Схемы append-only: изменение = V2 + dual-publish.

---

## 🚫 Отсутствуют (НЕ импортировать — пакетов нет в дереве)

Заблокированы пустым пин-коммитом (см. статус выше). Планируемый API — из ARCHITECTURE.md §9,
здесь для ориентира контекст-агентам; сигнатуры станут контрактом только после реализации.

| Пакет | Назначение | Ключевой планируемый API |
|---|---|---|
| `decorator` | generic cross-cutting на каждый хендлер | `CommandHandler[C]`, `QueryHandler[Q,R]`, `ApplyCommandDecorators[C](h, *slog.Logger, *MetricsClient, trace.Tracer)`, `ApplyQueryDecorators[Q,R](...)`; лог defer-ом на named err, RED `molot_command_duration_seconds{context,handler,result}`, span `commands/<Name>` |
| `metrics` | OTLP gRPC MeterProvider + runtime instrumentation, resource `service.name=molot` | `NewMeterProvider(ctx, endpoint) (*sdkmetric.MeterProvider, error)` |
| `tracing` | OTLP gRPC TracerProvider, W3C propagation | `NewTracerProvider(ctx, endpoint) (*sdktrace.TracerProvider, error)` |
| `postgres` | пул database/sql поверх pgx stdlib + otelsql; транзакции | `NewPool(ctx, databaseURL) (*sql.DB, error)`, `RunInTx(ctx, db, fn) error`, `FinishTransaction(err, tx) error` (multierr.Combine), goose-хелпер миграций |
| `auth` | JWT middleware (HS256 local; jwks — ошибка конфигурации со старта), типизированный user в ctx | `User{ID uuid.UUID; Role Role}`, `UserFromCtx(ctx) (User, error)`, `NewMiddleware(mode, secret) (func(http.Handler) http.Handler, error)`, `GenerateFakeJWT(secret, user, ttl)` — тот же код-путь для dev/тестов |
| `server` | chi-роутер, стек §8, healthz/readyz с инжектируемыми пробами, запуск/shutdown | `New(...)`, `RespondWithSlugError(err, w, r)` — маппинг ErrorKind→HTTP, тело `{"slug": "..."}` |
| `watermill` | router (CorrelationID→PoisonQueue("events.dead_letter")→Retry{5,exp≤30s}→Recoverer), SQL pub/sub v4 (outbox), cqrs bus/processor (`JSONMarshaler{GenerateName: cqrs.StructName}`), trace propagation через metadata | `NewRouter`, `NewSQLPublisher/NewSQLSubscriber`, `NewEventBus/NewEventProcessor`, `PublishInTx(ctx, tx, ...)` |

## Восстановленный пин-сет (для оркестратора)

Версии восстановлены по burst-загрузке module cache в момент коммита a5f939c (2026-06-10/11):

```
github.com/ThreeDotsLabs/watermill v1.5.2
github.com/ThreeDotsLabs/watermill-sql/v4 v4.1.5
github.com/go-chi/chi/v5 v5.3.0
github.com/jackc/pgx/v5 v5.10.0
github.com/pressly/goose/v3 v3.27.1
github.com/oapi-codegen/runtime v1.4.1
go.opentelemetry.io/otel v1.44.0 (+ trace, metric, sdk, sdk/metric v1.44.0)
go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.44.0
go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.44.0
go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0
go.opentelemetry.io/contrib/instrumentation/runtime v0.69.0
github.com/XSAM/otelsql v0.42.0        # не в burst — новейшая в кэше
github.com/golang-jwt/jwt/v5 v5.3.1    # не в burst — новейшая в кэше
github.com/google/uuid v1.6.0
go.uber.org/multierr v1.11.0
github.com/stretchr/testify v1.11.1    # уже в go.sum, нет в require
```
