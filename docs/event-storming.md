# Event Storming — артефакт discovery (правило 51)

> Границы контекстов выведены из доменного потока «лот → ставки → молоток → счёт → оплата/компенсация», не из «похожих существительных». Таблицы ниже — аудируемый маппинг «доменное событие / команда / агрегат → Go-тип / хендлер». При изменении домена сначала правится этот файл, потом код.

## Поток (big picture, хронология слева направо)

```
[Продавец]                [Участники]                [Время]              [Сага]                    [Деньги]
ListAuction ──► AuctionListed
                PlaceBid ──► BidPlaced (×N, анти-снайп: AuctionExtended)
CancelAuction ─► AuctionCancelled (до первой ставки)
                                          Close ──► AuctionClosed{sold|not_sold}
                                                         │ sold
                                                         ▼
                                                    Settlement.Start ──► IssueInvoice ──► InvoiceIssued
                                                                                          PayInvoice ──► InvoicePaid ──► ConfirmSettlement ──► SaleSettled
                                                                                          (таймаут) ──► InvoiceExpired ──► развилка NextStep:
                                                                                              ├─ AwardToRunnerUp ──► WinnerReassigned ──► IssueInvoice(attempt=2)
                                                                                              ├─ Relist ──► AuctionRelisted (новый агрегат, gen+1)
                                                                                              └─ MarkSaleFailed ──► SaleFailed
                                                                       DeclineSecondChanceOffer ──► VoidInvoice ──► та же развилка
```

## Команды → агрегат → хендлер

| Команда (бизнес-язык) | Актор | Агрегат | Go-тип | Хендлер |
|---|---|---|---|---|
| Выставить лот | продавец | Auction | `command.ListAuction` | `internal/auction/app/command/list_auction.go` |
| Сделать ставку | участник | Auction | `command.PlaceBid` | `internal/auction/app/command/place_bid.go` |
| Отменить лот | продавец | Auction | `command.CancelAuction` | `internal/auction/app/command/cancel_auction.go` |
| Закрыть по времени | система (воркер) | Auction | `command.CloseAuction` | `internal/auction/app/command/close_auction.go` |
| Переназначить победителя | сага | Auction | `command.AwardToRunnerUp` | `internal/auction/app/command/award_to_runner_up.go` |
| Перевыставить лот | сага | Auction (новый) | `command.RelistAuction` | `internal/auction/app/command/relist_auction.go` |
| Зафиксировать провал продажи | сага | Auction | `command.MarkSaleFailed` | `internal/auction/app/command/mark_sale_failed.go` |
| Подтвердить расчёт | сага | Auction | `command.ConfirmSettlement` | `internal/auction/app/command/confirm_settlement.go` |
| Зарегистрироваться | гость | Participant | `command.RegisterParticipant` | `internal/participant/app/command/register_participant.go` |
| Верифицировать участника | operations | Participant | `command.VerifyParticipant` | `internal/participant/app/command/verify_participant.go` |
| Выставить счёт | сага | Invoice | `command.IssueInvoice` | `internal/billing/app/command/issue_invoice.go` |
| Оплатить счёт | должник | Invoice | `command.PayInvoice` | `internal/billing/app/command/pay_invoice.go` |
| Просрочить счёт | система (воркер) | Invoice | `command.ExpireInvoice` | `internal/billing/app/command/expire_invoice.go` |
| Аннулировать счёт | сага | Invoice | `command.VoidInvoice` | `internal/billing/app/command/void_invoice.go` |
| Отклонить оферту second chance | runner-up | Settlement | `command.DeclineSecondChanceOffer` | `internal/settlement/app/command/decline_second_chance_offer.go` |

## Доменные события → интеграционные → подписчики

| Доменное событие | Агрегат | Интеграционное (V1) | Подписчики (хендлеры) |
|---|---|---|---|
| AuctionListed | Auction | `auctionevents.AuctionListedV1` | auction-проекции; notification (relist-gen>0 → продавцу) |
| BidPlaced | Auction | `auctionevents.BidPlacedV1` | auction-проекции; notification (OutbidNotice) |
| AuctionExtended | Auction | — (внутри BidPlacedV1: Extended, NewEndsAt) | — |
| AuctionCancelled | Auction | `auctionevents.AuctionCancelledV1` | auction-проекции |
| AuctionClosed | Auction | `auctionevents.AuctionClosedV1` | settlement (старт саги, sold); auction-проекции; notification |
| WinnerReassigned | Auction | `auctionevents.WinnerReassignedV1` | settlement (шаг саги); notification (SecondChanceOffer) |
| AuctionRelisted | Auction | `auctionevents.AuctionRelistedV1` | auction-проекции |
| SaleSettled | Auction | `auctionevents.SaleSettledV1` | auction-дашборд; notification (продавцу) |
| SaleFailed | Auction | `auctionevents.SaleFailedV1` | auction-дашборд; notification (продавцу, с reason) |
| InvoiceIssued | Invoice | `billingevents.InvoiceIssuedV1` | notification (PaymentDueNotice) |
| InvoicePaid | Invoice | `billingevents.InvoicePaidV1` | settlement; notification (квитанция) |
| InvoiceExpired | Invoice | `billingevents.InvoiceExpiredV1` | settlement |
| InvoiceVoided | Invoice | — (потребителей нет; не публикуется) | — |
| ParticipantRegistered | Participant | `participantevents.ParticipantRegisteredV1` | auction (bidder_profiles); notification (recipients) |
| ParticipantVerified | Participant | `participantevents.ParticipantVerifiedV1` | auction (bidder_profiles); notification (recipients) |

## Hotspots (зоны риска, выявленные на discovery)

1. **PlaceBid vs Close в снайп-окне** — гонка разрешается row lock + перечитка (`ErrBiddingStillOpen`); race-тест обязателен.
2. **Paid vs Expired** — разрешается на агрегате Invoice (guard-таблица); сага никогда не видит оба сигнала; проигравшая оплата компенсируется Refund.
3. **Charged-but-expired** — PSP Charge до транзакции + идемпотентный Refund (§3.1 ARCHITECTURE).
4. **Redelivery каждого шага саги** — протокол decide → effect → commit; идемпотентные фасады; crash-seam карта §6.7.
5. **Скрытность резервной цены** — наружу только `RunnerUpQualifies bool`; сам резерв не покидает агрегат.

## Литмус границ (правило 52)

- «Изменить шаг ставки» → только auction.
- «Изменить формулу комиссии» → только billing.
- «Новая ветка компенсации» → только settlement.
- «Новый канал уведомлений» → только notification.
- «Новое правило верификации» → participant (+ снапшот политики в auction при листинге).
