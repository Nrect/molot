# notification — контекст уведомлений

Тривиальный event-консьюмер: интеграционное событие → текст письма → отправка (фейковый email в slog).
HTTP API нет, фасада нет, OpenAPI не нужен.

## Исключение правила 33: без domain/app-слоёв

В контексте нет бизнес-инвариантов: ни состояния с переходами, ни команд, ни запросов.
Единственная «логика» — выбор шаблона, адресата и дедупликация доставки; она объявлена
декларативно в `ports/events.go` рядом с подписками. Вводить domain/app здесь — слои-пустышки
(агрегат без инвариантов, app-хендлер из одной строки), что чек-лист запрещает так же, как
и пропуск слоёв там, где они нужны.

**Условие пересмотра:** как только появляется поведение — пользовательские предпочтения
каналов/подписок, digest/батчинг, расписания тишины, повторные отправки, реальный SMTP с
ретраями — контекст получает полноценные domain/app по общим правилам, и это исключение
удаляется.

## Идемпотентность и trade-off «at-most-once after dedup»

- Ключ дедупликации — PK `(event_id, kind, recipient_id)` в `notification.sent_notifications`
  (графт G7/D6): одно событие может породить несколько писем (например, `AuctionClosedV1` →
  «won» победителю и «closed» продавцу), каждое — отдельный слот.
- Порядок доставки: **резолв получателя → INSERT … ON CONFLICT DO NOTHING → commit → send**.
  Отправка письма не транзакционна — объединить её с INSERT в одну транзакцию нельзя.
- Следствие: крах процесса между commit и send теряет ровно это письмо. Это осознанный
  trade-off **at-most-once-after-dedup**: при at-least-once-альтернативе (слать до записи)
  redelivery штамповал бы дубликаты писем, что для нотификаций хуже единичной потери.
  Честный at-least-once потребовал бы email-outbox с диспетчер-воркером — имеет смысл только
  вместе с реальным SMTP-адаптером (см. условие пересмотра выше).
- Резолв получателя выполняется **до** записи слота: иначе ошибка резолва после INSERT
  навсегда сожгла бы слот, не отправив письмо.
- Неизвестный получатель/продавец = лаг проекции → хендлер возвращает ошибку → ретраи шины
  (5 × exp backoff) → dead letter, если лаг не рассосался. Слот при этом не расходуется,
  поэтому redelivery доставляет письмо, как только проекция догонит.

## Мини-проекции (не read models)

| Таблица | Ключ → данные | Питается | Зачем |
|---|---|---|---|
| `notification.recipients` | participant_id → email, display_name | `ParticipantRegisteredV1` | адрес и имя для письма |
| `notification.auction_sellers` | auction_id → seller_id | `AuctionListedV1`, `AuctionClosedV1` | `SaleSettledV1`/`SaleFailedV1` адресуются продавцу, но seller_id **не несут** |

`auction_sellers` — дополнение к схеме §7 (там перечислены только sent_notifications и
recipients): без карты «аукцион → продавец» письма о расчёте адресовать нечем, а менять
схемы V1-событий нельзя (append-only).

## Подписки (§4.3); имя хендлера = consumer group

| Событие | Хендлер | Получатель | kind |
|---|---|---|---|
| ParticipantRegisteredV1 | OnParticipantRegisteredUpsertRecipient | — | upsert `recipients` |
| ParticipantVerifiedV1 | OnParticipantVerified | — | no-op: в payload только id; verified-флаг не нужен ни одному письму — колонка не заводится (правило 54) |
| BidPlacedV1 | OnBidPlacedNotifyOutbid | прежний лидер (`OutbidBidderID != ""`) | outbid_notice |
| AuctionListedV1 | OnAuctionListedNotifyRelist | продавец, только RelistGeneration > 0 | auction_relisted (всегда кормит `auction_sellers`) |
| AuctionClosedV1 | OnAuctionClosedNotifyOutcome | победитель (sold) + продавец | auction_won, auction_closed |
| WinnerReassignedV1 | OnWinnerReassignedOfferSecondChance | новый победитель | second_chance_offer |
| SaleSettledV1 | OnSaleSettledNotifySeller | продавец (из `auction_sellers`) | sale_settled |
| SaleFailedV1 | OnSaleFailedNotifySeller | продавец (из `auction_sellers`) | sale_failed |
| InvoiceIssuedV1 | OnInvoiceIssuedNotifyPaymentDue | должник | payment_due_notice |
| InvoicePaidV1 | OnInvoicePaidSendReceipt | должник | payment_receipt |

## Сборка

```go
svc := notificationservice.NewService(db, logger)            // паникует на nil
err := svc.RegisterEventHandlers(wmRouter, subscriberConstructor)
notificationservice.Migrations                               // fs.FS, корень = *.sql (как у остальных контекстов)
```

Для goose: `goose.NewProvider(goose.DialectPostgres, db, Migrations, goose.WithTableName("goose_db_version_notification"))`
(провайдер goose ищет `*.sql` в корне FS; сырой embed.FS — `adapters.Migrations`).

Процессор — стандартный `common/watermill.NewEventProcessor`: он ставит
`AckOnUnknownEvent: true`, так что на мульти-событийных топиках (auction-events
несёт 8 типов, потребляем 6) чужие типы события ack-аются, а не гоняются
по ретраям в dead letter.

## Тесты

- `ports` — юнит, recording spies, без Docker: идемпотентность redelivery каждого письма,
  fan-out одного события на двух получателей, до-доставка только недостающего письма после
  частичного краха, «лаг проекции не сжигает слот», полная матрица подписок.
- `adapters` — один shared suite на pg+inmem: pg-половина под build tag `integration`,
  скипается без `TEST_DATABASE_URL` (поднять: `docker compose up -d postgres`); race-тест
  `FirstDelivery` (20 горутин, ровно один победитель). Rollback-теста нет: в контексте нет
  updateFn/транзакционных потоков — только одиночные идемпотентные INSERT/UPSERT.
