# 17. Эволюция без переписывания: отложенные решения за интерфейсами

## Зачем читать

Большинство систем не умирают от неправильного выбора технологии — они умирают от страха его поменять.
Страх появляется тогда, когда технологическое решение вшито в домен: чтобы переехать с одной очереди
на другую, приходится переписывать бизнес-логику. Этой главы не было бы, если бы в Molot всё было
спроектировано «под будущее» — написан код для Kafka, которой ещё нет, или созданы gRPC-интерфейсы
для сервисов, которые пока не нужны. Вместо этого здесь применяется другой принцип: **не код на
будущее, а дешёвые точки входа для будущего**. Разница критична. Код на будущее — это сложность
сегодня ради гипотетической задачи. Точки входа — это минимальный контракт, за которым технология
остаётся невидимой для всего остального кода.

Глава разбирает четыре конкретных эволюции по одной схеме: что меняется, что не меняется, почему
граница держит. В каждом случае — реальный код из репозитория, критерий «пора», типичные ошибки.

---

## Проблема: технология просачивается в домен

Классический сценарий выглядит так. Система запускается на простом решении — очередь в памяти,
синхронные вызовы между модулями, одна база данных для чтения и записи. Со временем появляется
нагрузка, и кто-то принимает решение «надо переходить на Kafka / выносить сервис / добавлять реплику».
Оказывается, что изменение невозможно сделать локально: Kafka-топики нужны в логике ретраев,
имена сервисов — в бизнес-транзакциях, первичная БД — в коде проекций. Переход становится полным
переписыванием, потому что технологические детали успели прорасти через все слои.

Корень проблемы — отсутствие стабильных интерфейсов на границах, где могут появиться новые решения.
Нестабильное (транспорт, хранилище, топология деплоя) смешалось со стабильным (бизнес-правила,
контракты событий, логика саги).

---

## Теория: граница держит, когда меняется одна вещь

Принцип прост: технологическое решение не должно просачиваться дальше одного слоя. Если оно
остаётся за конструктором, за интерфейсом или за composition root — замена стоит изменения
одного файла. Если оно расползлось — замена стоит рефакторинга всей системы.

Хорошая точка входа для будущего решения удовлетворяет трём условиям:
1. Её контракт стабилен и не содержит деталей реализации за ней.
2. Существующий код ничего не знает о том, что за интерфейсом.
3. Новая реализация встаёт на место старой без изменения потребителей.

Именно это имеется в виду, когда говорят «транспорт — деталь» или «хранилище — деталь». Не
философский принцип, а конкретное инженерное утверждение: если завтра появится другой транспорт,
никакой код снаружи адаптера не узнает об этом.

---

## Как подготовлено в Molot

### Эволюция 1: Kafka вместо watermill-sql

#### Что меняется

Publisher и subscriber — конкретные реализации из библиотеки `watermill-sql`. Сегодня они пишут
сообщения в Postgres-таблицы и читают их polling-ом. Kafka-реализации пишут в партиции и читают
через consumer groups.

#### Что не меняется

Контракты событий (`events/`), типизированные хендлеры, middleware-цепочка router-а, идемпотентность
хендлеров — ничего из этого не знает, откуда пришло сообщение.

#### Почему граница держит

Всё, что зависит от конкретного транспорта, сосредоточено в трёх конструкторах одного файла.
`internal/common/watermill/pubsub.go`:

```go
// NewSQLSubscriber builds a Postgres subscriber for one consumer group.
// Consumer groups track offsets independently, so each event handler
// (group = handler name, see NewEventProcessor) receives every message.
func NewSQLSubscriber(db *sql.DB, consumerGroup string, pollInterval time.Duration,
    logger wm.LoggerAdapter) (message.Subscriber, error) {
    subscriber, err := wmsql.NewSubscriber(
        wmsql.BeginnerFromStdSQL(db),
        wmsql.SubscriberConfig{
            ConsumerGroup:    consumerGroup,
            SchemaAdapter:    wmsql.DefaultPostgreSQLSchema{},
            OffsetsAdapter:   wmsql.DefaultPostgreSQLOffsetsAdapter{},
            InitializeSchema: true,
            PollInterval:     pollInterval,
        },
        logger,
    )
    ...
    return subscriber, nil
}

// NewTxPublisher builds a publisher bound to an open *sql.Tx for outbox
// publication: the event INSERT commits or rolls back together with the
// business write.
func NewTxPublisher(tx *sql.Tx, logger wm.LoggerAdapter) (message.Publisher, error) {
    publisher, err := wmsql.NewPublisher(
        wmsql.TxFromStdSQL(tx),
        wmsql.PublisherConfig{
            SchemaAdapter:        wmsql.DefaultPostgreSQLSchema{},
            AutoInitializeSchema: false,
        },
        logger,
    )
    ...
    return NewTracingPublisherDecorator(publisher), nil
}
```

Возвращаемые типы — `message.Subscriber` и `message.Publisher`, интерфейсы watermill, а не
конкретные типы. Весь остальной код, включая `app.go`, получает только эти интерфейсы. Замена
на Kafka означает замену тела этих трёх функций: `wmsql.NewSubscriber` → `kafka.NewSubscriber`,
`wmsql.TxFromStdSQL(tx)` → соответствующий Kafka-продьюсер через Watermill forwarder. Хендлеры,
контракты событий, router — ни одна строка снаружи этого файла не меняется.

ADR-0002 явно фиксирует это как решение: «Миграция на Kafka = замена publisher/subscriber
(+ Watermill forwarder как мост); контракты `events/`, хендлеры и тесты не трогаются».

**Когда пора.** Метрика — не «все так делают», а конкретные числа: `molot_bus_oldest_message_age_seconds`
растёт, poll-lag стабильно отстаёт от продакшен-темпа, и это проявляется на реальной нагрузке.
На сотнях сообщений в секунду watermill-sql справляется; Kafka нужна при тысячах в секунду
с требованием гарантированного порядка по ключу агрегата. До того момента операционные расходы
Kafka (отдельный кластер, управление партициями, schema registry) не оправданы.

---

### Эволюция 2: вынос контекста в отдельный сервис

#### Что меняется

Рассмотрим billing как первый кандидат: собственная схема БД, собственные миграции, отдельный
процесс, HTTP/gRPC-эндпоинт вместо вызова через in-process фасад.

#### Что не меняется

Домен billing, его use cases, события `BillingV1`, хендлеры на стороне settlement — ничего из
этого не затронуто. Меняется только то, что находится между двумя контекстами.

#### Почему граница держит

Settlement-сага вызывает billing не напрямую, а через consumer-defined интерфейс. Посмотрим
на `internal/settlement/app/handlers.go`:

```go
// Consumer-side gateways (rule 5, §6, ADR-0004): the slice of the
// foreign sync facades the saga's event handlers need. Adapters bridge
// them to auctionservice.Facade / billingservice.Facade; every command
// is idempotent on the far side ("already in the target state" → nil).
type billingGateway interface {
    IssueInvoice(ctx context.Context, invoiceID settlement.InvoiceID, auctionID settlement.AuctionID,
        debtor settlement.BidderID, amount settlement.Money, attempt int) error
}
```

Этот интерфейс объявлен внутри пакета `app` settlement-контекста. Сегодня за ним стоит
`BillingFacadeAdapter` — тонкая обёртка над in-process фасадом (`internal/settlement/adapters/billing_facade_adapter.go`):

```go
// BillingFacadeAdapter implements the saga's consumer-side billing
// gateways over the synchronous facade with facade/ spans (§11).
type BillingFacadeAdapter struct {
    facade BillingFacade
    tracer trace.Tracer
}

// IssueInvoice issues the attempt's bill. Idempotent on the billing
// side: the deterministic invoice id and UNIQUE(auction_id, attempt)
// turn a repeat into a no-op (nil).
func (a BillingFacadeAdapter) IssueInvoice(
    ctx context.Context,
    invoiceID settlement.InvoiceID,
    auctionID settlement.AuctionID,
    debtor settlement.BidderID,
    amount settlement.Money,
    attempt int,
) error {
    return a.traced(ctx, "facade/billing.IssueInvoice", func(ctx context.Context) error {
        return a.facade.IssueInvoice(ctx,
            invoiceID.UUID(), auctionID.UUID(), debtor.UUID(),
            amount.Amount(), amount.Currency().String(), attempt,
        )
    })
}
```

При выносе billing в отдельный сервис заменяется только этот адаптер. `BillingFacadeAdapter`
превращается в `BillingGRPCAdapter` с тем же набором методов. Интерфейс `billingGateway` в app-слое
settlement остаётся нетронутым — сага об этом ничего не знает.

**Пошаговый рецепт выноса:**
1. Billing получает собственный DSN и схему. Миграции уже изолированы (`goose_db_version_billing`).
2. Billing поднимается как отдельный бинарь `cmd/billing` с собственным composition root.
3. `BillingFacadeAdapter` заменяется `BillingGRPCAdapter` — settlement вызывает billing по gRPC.
4. Сагу не трогаем. Идемпотентность уже есть: детерминированный `invoiceID` и `UNIQUE(auction_id, attempt)`
   превращают сетевой retry в no-op на стороне billing.

Обратите внимание на последний пункт: идемпотентность billing не появляется при переходе
к gRPC — она уже есть, потому что at-least-once semantics заложена с первого дня. Сеть добавляет
повторные доставки, но они обрабатываются ровно так же, как сейчас обрабатываются повторные
доставки событий.

**Промежуточный шаг — selective deployment (ROADMAP §7.2.3).** Прежде чем делать gRPC, можно
вынести контекст в отдельный бинарь, оставив шину общей. `cmd/monolith` без notification плюс
отдельный `cmd/notifier` только с ним — два бинаря из тех же Go-модулей, разные composition root,
общая watermill-sql. Это доказывает, что контексты реально независимы, без введения сетевого
взаимодействия между ними.

В `internal/monolith/app.go` хорошо видно, как вся сборка умещается в одной функции — единственное
место, где конкретные типы встречаются друг с другом:

```go
settlementSvc, err := settlementservice.NewService(settlementservice.Deps{
    DB:             db,
    Logger:         logger,
    MeterProvider:  meterProvider,
    TracerProvider: tracerProvider,
    AuctionFacade:  auctionSvc.Facade(),
    BillingFacade:  billingSvc.Facade(),
    RelistDelay:    cfg.RelistDelay,
    RelistDuration: cfg.RelistDuration,
})
```

`AuctionFacade: auctionSvc.Facade()` — единственная строка, связывающая settlement и auction.
При selective deployment для notifier эта строка просто исчезает из `cmd/notifier/main.go`.
При выносе billing в микросервис она заменяется на `BillingFacade: grpc.NewBillingClient(cfg.BillingAddr)`.

**Когда пора.** Два критерия — факты, не мода: (1) billing нужно деплоить независимо от аукционного
ядра (разные команды, разный темп релизов, разные требования по надёжности); (2) billing упирается
в ресурсы, которые нельзя масштабировать репликой целого монолита. До этого момента сетевой
вызов добавляет latency, точку отказа и distributed tracing накладные расходы — без компенсирующей выгоды.

---

### Эволюция 3: read replicas и отдельный read store

#### Что меняется

DSN, из которого читают query-хендлеры. Вместо primary Postgres — реплика или отдельный
поисковый движок.

#### Что не меняется

Read-model интерфейсы, query-хендлеры, HTTP-порты. Читателю не важно, откуда пришли данные.

#### Почему граница держит

Каждый query-хендлер объявляет минимальный consumer-side read-model интерфейс. Из
`internal/auction/app/query/active_catalog.go`:

```go
type ActiveCatalogReadModel interface {
    ActiveAuctions(ctx context.Context, page, pageSize int) (CatalogPage, error)
}

type ActiveAuctionsCatalogHandler struct {
    readModel ActiveCatalogReadModel
}
```

Из `internal/auction/app/query/auction_card.go`:

```go
type AuctionCardReadModel interface {
    AuctionCard(ctx context.Context, id auction.AuctionID) (AuctionCardView, error)
}
```

Из `internal/settlement/app/query/settlement_status.go`:

```go
// SettlementReadModel reads the saga state for operations.
type SettlementReadModel interface {
    SettlementByAuction(ctx context.Context, id settlement.AuctionID) (SettlementView, error)
}
```

За каждым из этих интерфейсов сегодня стоит реализация на Postgres-primary
(`AuctionPostgresReadModels`). Для каталога достаточно поменять DSN в composition root на
реплику — интерфейс `ActiveCatalogReadModel` удовлетворит та же структура, но подключённая к
реплике. Для полнотекстового поиска появляется новая реализация `ElasticCatalogReadModel`,
удовлетворяющая тому же интерфейсу; подключается в composition root заменой одной строки.

Ставки и сага — всегда primary. Это правило не в конфигурации — оно в коде: write-пути
(`UpdateFn`, `saga.Update`) получают `*sql.DB`, подключённый к primary, и этот DSN не меняется.

**Когда пора.** Чтения каталога и карточки превышают записи на один-два порядка — это проявляется
в метриках latency и saturation пула соединений primary. Реплика нужна именно тогда, когда primary
сигнализирует о перегрузке на read-трафик, а не превентивно.

---

### Эволюция 4: горизонтальное масштабирование реплик

#### Что меняется

Количество инстансов приложения за балансировщиком.

#### Что не меняется

Код. Ни один файл не правится.

#### Почему граница держит

Система уже stateless. JWT, состояние в Postgres, файлы в S3 — ничего, что мешало бы поднять
вторую реплику. ADR-0001 явно фиксирует: «масштабирование — только репликами целого бинаря;
воркеры и event-хендлеры спроектированы под конкурентные реплики (row lock + guard-ы агрегатов)».

Конкретно: две реплики одновременно получат одно и то же watermill-сообщение (at-least-once) и
обе попытаются выполнить `Update` на одном агрегате. Одна захватит `SELECT ... FOR UPDATE` и
успешно зафиксирует переход. Вторая увидит уже изменённый `version`, получит
`ErrUnexpectedTransition` и сделает ack — дубль обработан корректно. Это не случайное поведение,
а спроектированное: все race-tests в сьюте проверяют именно эти гонки.

**Когда пора.** CPU или RAM primary-инстанса систематически выше 70% под нагрузкой, либо latency
p99 command-пути растёт. Первый шаг — вертикальный (ресурсы одного инстанса); горизонтальный —
когда вертикаль исчерпана или экономически нецелесообразна.

---

## Трейдоффы

**Интерфейсный overhead.** Каждый consumer-defined интерфейс — это дополнительный уровень
косвенности. В критически горячем пути это иногда заметно. В Molot все фасадные вызовы
(сага → billing, сага → auction) происходят вне транзакций, в effect-фазе, то есть не на
критическом пути агрегатной записи. Накладные расходы минимальны.

**Selective deployment усложняет инфраструктуру.** Два бинаря означают два процесса, два
набора health checks, два места деплоя. До определённого масштаба это чистый минус, после —
независимость деплоя оправдывает затраты.

**Read replica добавляет replication lag.** Каталог может показывать данные с задержкой.
Для аукциона это приемлемо — пользователь, только что сделавший ставку, читает карточку
напрямую с primary (read-your-writes); анонимный просмотр каталога lag не заметит.

---

## Типичные ошибки

**Kafka «потому что все так делают».** Kafka нужна при конкретных числах: тысячи сообщений
в секунду, требование гарантированного порядка по ключу агрегата на уровне брокера, несколько
независимых consumer groups с разными требованиями по latency. Если этих чисел нет — операционная
стоимость кластера, управление schema registry, настройка partition key routing — всё это ложится
на команду без компенсирующей выгоды. ADR-0002 явно обосновал выбор watermill-sql: «сотни
msg/s максимум — Postgres уже есть и держит состояние».

**Вынос сервиса без собственной БД.** Самая частая ошибка при первом шаге к микросервисам:
billing живёт в отдельном процессе, но читает из той же схемы Postgres, что и монолит. Граница
есть на сетевом уровне, но нет на уровне данных. Это distributed monolith: любая миграция
схемы требует координации деплоев, JOIN через сеть, общие connection pools. Вынос контекста
имеет смысл только вместе с выносом схемы. В Molot миграции billing изолированы с первого дня
(`goose_db_version_billing` — отдельная таблица версий, `billing.*` — отдельная Postgres-схема).

**Шардирование до исчерпания реплик и кэша.** Шардирование — последний резерв, не первый.
Порядок: кэш каталога (singleflight + TTL), read replica для query-стороны, вертикальный рост,
горизонтальный рост (реплики монолита) — и только потом шардирование по `auction_id`. HIGHLOAD.md
прямо об этом: «Последний резерв — после реплик, партиций, кэша». Шардирование означает потерю
кросс-шардовых JOIN, что требует денормализации событий и существенной переработки read-моделей.

**Выносить сервис без чёткого организационного запроса.** «Billing должен масштабироваться
независимо» — это инженерное утверждение, которое требует доказательства: метрики показывают,
что billing является узким местом, и это узкое место нельзя устранить репликой монолита или
кэшем. Без этого доказательства вынос — усложнение ради усложнения.

---

## Чек-лист: готов ли код к следующей эволюции

**Kafka:**
- [ ] Publisher и subscriber скрыты за `message.Publisher` / `message.Subscriber` watermill
- [ ] Конструкторы шины сосредоточены в одном пакете (`common/watermill`)
- [ ] Хендлеры идемпотентны по натуральному ключу (проверено тестом на повторную доставку)
- [ ] Метрики lag и dead-letter собираются и алертят

**Вынос контекста:**
- [ ] Фасадные вызовы скрыты за consumer-defined interface в app-слое (не в domain)
- [ ] Схема БД изолирована (`schema.*`, отдельная таблица миграций)
- [ ] Все команды через фасад идемпотентны («уже в целевом состоянии» → nil)
- [ ] Сага не импортирует domain чужого контекста напрямую

**Read replica / read store:**
- [ ] Каждый query-хендлер зависит от минимального read-model interface, объявленного рядом с ним
- [ ] Read-пути и write-пути получают DSN из разных параметров composition root
- [ ] Write-пути (агрегаты, сага) явно используют primary

**Горизонтальное масштабирование:**
- [ ] Нет состояния в памяти приложения (сессии, кэши — внешние)
- [ ] Все воркеры используют `SELECT ... FOR UPDATE` или advisory lock
- [ ] Race-тесты проверяют конкурентную обработку одного агрегата

---

## Ссылки

- ADR-0001 — Модульный монолит: `docs/adr/0001-modular-monolith.md`
- ADR-0002 — Watermill-sql bus: `docs/adr/0002-watermill-sql-bus.md`
- ROADMAP §4.5 — Kafka-транспорт: `docs/ROADMAP.md`
- ROADMAP §7.2.3 — Selective deployment: `docs/ROADMAP.md`
- HIGHLOAD.md §1.2 — Read replicas, §1.4 — Шардирование, §5.1 — Stateless app: `docs/HIGHLOAD.md`
- TEXTBOOK.md гл.15 — План эволюции: `docs/TEXTBOOK.md`
- `internal/common/watermill/pubsub.go` — конструкторы шины
- `internal/settlement/adapters/billing_facade_adapter.go` — фасадный адаптер саги
- `internal/settlement/adapters/auction_facade_adapter.go` — фасадный адаптер саги
- `internal/settlement/app/handlers.go` — consumer-defined gateways
- `internal/auction/app/query/` — read-model интерфейсы
- `internal/monolith/app.go` — composition root, точка сборки
