# 17. Эволюция без переписывания: отложенные решения за интерфейсами

## Зачем читать

Большинство систем умирают не от неправильного выбора технологии — они умирают от страха его
поменять. Страх появляется, когда технологическое решение вшито в домен: чтобы переехать с одной
очереди на другую, нужно переписать бизнес-логику. В этот момент команда обычно решает «живём
как есть» — и система начинает стареть.

Этой главы не было бы, если бы в Molot всё было спроектировано «под будущее»: написан код для
Kafka, которой ещё нет, созданы gRPC-интерфейсы для сервисов, которые пока не нужны. Здесь
применяется другой принцип, и его стоит запомнить: **не код на будущее, а дешёвые
точки входа для будущего**. Код на будущее — сложность сегодня ради гипотетической задачи,
и оплачивается она каждый день, пока лежит в репозитории. Точка входа — минимальный контракт,
за которым технология остаётся невидимой для остального кода: почти бесплатно сейчас, ровно
столько усилий, сколько нужно, потом.

Глава разбирает четыре конкретные эволюции по одной схеме: что меняется, что не меняется,
почему граница держит. В каждом случае — реальный код из репозитория, критерий «пора»
и типичные ошибки.

---

## Проблема: технология просачивается в домен

Вы наверняка проходили этот сценарий. Система запускается на простом решении: очередь в памяти,
синхронные вызовы между модулями, одна база на чтение и запись. Год всё хорошо. Потом приходит
нагрузка, и на планировании звучит: «надо переходить на Kafka» — или «выносить сервис», или
«добавлять реплику». Задача выглядит инфраструктурной — неделя. А через два дня выясняется:
Kafka-топики упоминаются в логике ретраев, имена сервисов зашиты в бизнес-транзакции, код
проекций знает, что база — именно primary. «Неделя» превращается в квартал, а квартал —
в «давайте не сейчас».

Корень всегда один: на границах, где могли появиться новые решения, не оказалось стабильных
интерфейсов. Нестабильное — транспорт, хранилище, топология деплоя — смешалось со стабильным:
бизнес-правилами, контрактами событий, логикой саги. Это как проводка, замурованная в несущую
стену: пока работает, никто не вспоминает, но менять придётся вместе со стеной.

---

## Теория: граница держит, когда меняется одна вещь

Принцип, на котором стоит вся глава, помещается в одно предложение: технологическое решение
не должно просачиваться дальше одного слоя. Если оно остаётся за конструктором, за интерфейсом
или за composition root — замена стоит изменения одного файла. Если расползлось — замена стоит
рефакторинга всей системы. Промежуточного не бывает: граница либо держит, либо нет.

Как отличить настоящую точку входа от декоративной? Три условия:
1. Её контракт стабилен и не содержит деталей реализации за ней.
2. Существующий код ничего не знает о том, что за интерфейсом.
3. Новая реализация встаёт на место старой без изменения потребителей.

Третье условие — самое честное: его можно проверить, а не обсудить. Именно это имеется в виду,
когда говорят «транспорт — деталь» или «хранилище — деталь»: если завтра появится другой
транспорт, никакой код снаружи адаптера об этом не узнает. Дальше мы проверим это утверждение
четыре раза подряд.

---

## Как подготовлено в Molot

### Эволюция 1: Kafka вместо watermill-sql

#### Что меняется

Publisher и subscriber — конкретные реализации из библиотеки `watermill-sql`. Сегодня они пишут
сообщения в Postgres-таблицы и читают их polling-ом. Kafka-реализации пишут в партиции и читают
через consumer groups.

#### Что не меняется

Контракты событий (`events/`), типизированные хендлеры, middleware-цепочка router-а,
идемпотентность хендлеров. Ничего из этого не знает, откуда пришло сообщение, — и в этом
незнании вся ценность.

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

На что смотреть: на возвращаемые типы. `message.Subscriber` и `message.Publisher` — интерфейсы
watermill, а не конкретные wmsql-типы. Весь остальной код, включая `app.go`, получает только эти
интерфейсы: подписчик «помнит», что он Postgres, ровно до выхода из конструктора. Замена на Kafka
означает замену тела этих трёх функций: `wmsql.NewSubscriber` → `kafka.NewSubscriber`,
`wmsql.TxFromStdSQL(tx)` → соответствующий Kafka-продьюсер через Watermill forwarder. Хендлеры,
контракты событий, router — ни одна строка снаружи этого файла не меняется.

ADR-0002 явно фиксирует это как решение: «Миграция на Kafka = замена publisher/subscriber
(+ Watermill forwarder как мост); контракты `events/`, хендлеры и тесты не трогаются».

**Когда пора.** Метрика — не «все так делают», а конкретные числа: `molot_bus_oldest_message_age_seconds`
растёт, poll-lag стабильно отстаёт от продакшен-темпа, и это проявляется на реальной нагрузке.
На сотнях сообщений в секунду watermill-sql справляется; Kafka нужна при тысячах в секунду
с требованием гарантированного порядка по ключу агрегата. До того момента операционные расходы
Kafka — отдельный кластер, управление партициями, schema registry — не оправданы.

**Совет из практики.** Не ждите миграции, чтобы проверить границу. Подмените реализацию
на in-memory-заглушку в отдельной ветке: если за вечер не вышло, у вас не граница, а пунктирная
линия на диаграмме — и лучше узнать это в спокойный вторник, чем в квартал переезда.

---

### Эволюция 2: вынос контекста в отдельный сервис

#### Что меняется

Рассмотрим billing как первый кандидат на вынос: собственная схема БД, собственные миграции,
отдельный процесс, HTTP/gRPC-эндпоинт вместо вызова через in-process фасад.

#### Что не меняется

Домен billing, его use cases, события `BillingV1`, хендлеры на стороне settlement — ничего из
этого не затронуто. Меняется только то, что находится между двумя контекстами.

#### Почему граница держит

Settlement-сага вызывает billing не напрямую, а через consumer-defined интерфейс — объявленный
у потребителя и описывающий только то, что ему нужно. Посмотрим
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

На что смотреть: интерфейс объявлен внутри пакета `app` settlement-контекста и состоит из одного
метода — ровно тот срез чужой функциональности, который нужен саге. Сегодня за ним стоит
`BillingFacadeAdapter` — тонкая обёртка над in-process фасадом
(`internal/settlement/adapters/billing_facade_adapter.go`):

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

Этот адаптер нарочито скучен — переводит типы и оборачивает вызов в span. Именно скучность
делает его заменяемым: при выносе billing в отдельный сервис меняется только он.
`BillingFacadeAdapter` превращается в `BillingGRPCAdapter` с тем же набором методов, а интерфейс
`billingGateway` в app-слое settlement остаётся нетронутым — сага так и не узнает, что billing
переехал в другой процесс.

**Пошаговый рецепт выноса:**
1. Billing получает собственный DSN и схему. Миграции уже изолированы (`goose_db_version_billing`).
2. Billing поднимается как отдельный бинарь `cmd/billing` с собственным composition root.
3. `BillingFacadeAdapter` заменяется `BillingGRPCAdapter` — settlement вызывает billing по gRPC.
4. Сагу не трогаем. Идемпотентность уже есть: детерминированный `invoiceID` и `UNIQUE(auction_id, attempt)`
   превращают сетевой retry в no-op на стороне billing.

Последний пункт — самый важный: идемпотентность billing не появляется при переходе к gRPC,
она уже есть, потому что at-least-once semantics заложена с первого дня.

**Нюанс.** Идемпотентность нельзя «добавить при переходе на gRPC» — к этому моменту она либо
уже есть, либо уже поздно. Сеть не создаёт новый класс проблем, она повышает частоту старых:
повторная доставка, редкая в монолите, по сети станет ежеминутной. Если команды через ваш фасад
не идемпотентны сегодня, вынос сервиса превратит редкий баг в постоянный фон.

**Промежуточный шаг — selective deployment (ROADMAP §7.2.3).** Прежде чем делать gRPC, можно
вынести контекст в отдельный бинарь, оставив шину общей: `cmd/monolith` без notification плюс
отдельный `cmd/notifier` только с ним — два бинаря из тех же Go-модулей, разные composition root,
общая watermill-sql. Это дешёвый способ доказать, что контексты реально независимы, не вводя
сетевого взаимодействия между ними.

В `internal/monolith/app.go` хорошо видно, как вся сборка умещается в одной функции —
единственное место, где конкретные типы встречаются друг с другом:

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

На что смотреть: `AuctionFacade: auctionSvc.Facade()` — единственная строка, связывающая
settlement и auction. При selective deployment для notifier эта строка просто исчезает из
`cmd/notifier/main.go`. При выносе billing в микросервис она заменяется
на `BillingFacade: grpc.NewBillingClient(cfg.BillingAddr)`.

**Когда пора.** Два критерия, и оба — факты, а не мода: (1) billing нужно деплоить независимо
от аукционного ядра — разные команды, разный темп релизов, разные требования по надёжности;
(2) billing упирается в ресурсы, которые нельзя масштабировать репликой целого монолита. Пока
ни один не выполнен, сетевой вызов добавляет latency, точку отказа и накладные расходы
distributed tracing — без компенсирующей выгоды.

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

На что смотреть: интерфейс объявлен рядом с хендлером и содержит один метод — ровно то, что
хендлеру нужно. Тот же приём в `internal/auction/app/query/auction_card.go`:

```go
type AuctionCardReadModel interface {
    AuctionCard(ctx context.Context, id auction.AuctionID) (AuctionCardView, error)
}
```

И в `internal/settlement/app/query/settlement_status.go`:

```go
// SettlementReadModel reads the saga state for operations.
type SettlementReadModel interface {
    SettlementByAuction(ctx context.Context, id settlement.AuctionID) (SettlementView, error)
}
```

Три хендлера — три крошечных интерфейса вместо одного «общего read-слоя»: каждый можно увести
на свой источник данных независимо от соседей. За каждым сегодня стоит реализация
на Postgres-primary (`AuctionPostgresReadModels`). Для каталога достаточно поменять DSN
в composition root на реплику — интерфейс `ActiveCatalogReadModel` удовлетворит та же структура,
но подключённая к реплике. Для полнотекстового поиска появляется новая реализация
`ElasticCatalogReadModel`, удовлетворяющая тому же интерфейсу; подключается заменой одной строки
в composition root.

Ставки и сага — всегда primary. Это правило не в конфигурации — оно в коде: write-пути
(`UpdateFn`, `saga.Update`) получают `*sql.DB`, подключённый к primary, и этот DSN не меняется.

**Где вы на это наступите.** Replication lag — не абстрактный трейдофф, а конкретный тикет:
«сделал ставку, обновил страницу — ставки нет». На стейдже реплика не отстаёт — всё зелёное;
на проде отставание в секунды превращает каталог в машину времени. Правило «кто только что
писал — читает с primary» (read-your-writes) проектируют до включения реплики, а не после
первого такого тикета.

**Когда пора.** Чтения каталога и карточки превышают записи на один-два порядка — это видно
в метриках latency и saturation пула соединений primary. Реплика нужна тогда, когда primary
сигнализирует о перегрузке на read-трафик, а не превентивно.

---

### Эволюция 4: горизонтальное масштабирование реплик

#### Что меняется

Количество инстансов приложения за балансировщиком.

#### Что не меняется

Код. Ни один файл не правится.

#### Почему граница держит

Система уже stateless. JWT, состояние в Postgres, файлы в S3 — в памяти процесса не живёт
ничего, что помешало бы поднять вторую реплику. ADR-0001 явно фиксирует: «масштабирование —
только репликами целого бинаря; воркеры и event-хендлеры спроектированы под конкурентные
реплики (row lock + guard-ы агрегатов)».

Самый неприятный сценарий: две реплики одновременно получают одно и то же
watermill-сообщение (at-least-once), и обе пытаются выполнить `Update` на одном агрегате.
Одна захватывает `SELECT ... FOR UPDATE` и успешно фиксирует переход. Вторая видит уже
изменённый `version`, получает `ErrUnexpectedTransition` и делает ack — дубль обработан
корректно. Это не удачное стечение обстоятельств, а спроектированное поведение: все race-tests
в сьюте проверяют именно эти гонки.

**Когда пора.** CPU или RAM primary-инстанса систематически выше 70% под нагрузкой, либо растёт
latency p99 command-пути. Первый шаг — вертикальный (ресурсы одного инстанса); горизонтальный —
когда вертикаль исчерпана или экономически нецелесообразна.

---

## Трейдоффы

Честная архитектура знает, за что заплатила. У отложенных решений цена тоже есть — три позиции.

**Интерфейсный overhead.** Каждый consumer-defined интерфейс — дополнительный уровень
косвенности, и в критически горячем пути это иногда заметно. В Molot все фасадные вызовы
(сага → billing, сага → auction) происходят вне транзакций, в effect-фазе, то есть не на
критическом пути агрегатной записи. Накладные расходы минимальны — но это свойство конкретного
дизайна, а не интерфейсов вообще.

**Selective deployment усложняет инфраструктуру.** Два бинаря — это два процесса, два набора
health checks, два места деплоя. До определённого масштаба это чистый минус, после —
независимость деплоя оправдывает затраты. Граница «до/после» проходит не по числу строк кода,
а по числу команд и темпов релизов.

**Read replica добавляет replication lag.** Каталог может показывать данные с задержкой.
Для аукциона это приемлемо — пользователь, только что сделавший ставку, читает карточку
напрямую с primary (read-your-writes); анонимный просмотр каталога lag не заметит.
Но «приемлемо» — решение продукта, и его проговаривают заранее.

---

## Типичные ошибки

**Kafka «потому что все так делают».** На архитектурном комитете слово «Kafka» звучит весомо —
и в этом его главная опасность. Kafka нужна при конкретных числах: тысячи сообщений в секунду,
требование гарантированного порядка по ключу агрегата на уровне брокера, несколько независимых
consumer groups с разными требованиями по latency. Если этих чисел нет — операционная стоимость
кластера, управление schema registry, настройка partition key routing — всё это ложится на
команду без компенсирующей выгоды. ADR-0002 явно обосновал выбор watermill-sql: «сотни
msg/s максимум — Postgres уже есть и держит состояние».

**Вынос сервиса без собственной БД.** Самая частая ошибка первого шага к микросервисам:
billing живёт в отдельном процессе, но читает из той же схемы Postgres, что и монолит. Граница
есть на сетевом уровне — и отсутствует на уровне данных, то есть по сути отсутствует вовсе.
Это distributed monolith: любая миграция схемы требует координации деплоев, JOIN ходит через
сеть, connection pools общие. Вынос контекста имеет смысл только вместе с выносом схемы.
В Molot миграции billing изолированы с первого дня (`goose_db_version_billing` — отдельная
таблица версий, `billing.*` — отдельная Postgres-схема).

**Шардирование до исчерпания реплик и кэша.** Шардирование — последний резерв, не первый,
потому что оно единственное необратимо дорогое: теряются кросс-шардовые JOIN, события приходится
денормализовывать, read-модели — существенно переписывать. Порядок такой: кэш каталога
(singleflight + TTL), read replica для query-стороны, вертикальный рост, горизонтальный рост
(реплики монолита) — и только потом шардирование по `auction_id`. HIGHLOAD.md прямо об этом:
«Последний резерв — после реплик, партиций, кэша».

**Выносить сервис без чёткого организационного запроса.** «Billing должен масштабироваться
независимо» — это инженерное утверждение, и оно требует доказательства: метрики показывают,
что billing — узкое место, и это узкое место нельзя устранить репликой монолита или кэшем.
Без доказательства вынос — усложнение ради усложнения: все затраты распределённой системы,
ни одной из её выгод.

---

## Чек-лист: готов ли код к следующей эволюции

Перед каждой из четырёх эволюций пройдитесь по своему блоку. Если хоть один пункт не выполняется —
сначала почините границу, потом планируйте миграцию: в обратном порядке выйдет дороже.

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
