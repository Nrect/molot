# Глава 11. Деньги: корректность под отказами

## Зачем эта глава

Деньги — единственные данные в системе, за ошибки в которых платят буквально.
Потерянная ставка — неприятно. Потерянное событие — болезненно. Списанные дважды
10 000 евро — это возврат, разбор с PSP, иногда chargeback и почти всегда —
потерянный клиент. Поэтому платёжный код проектируют иначе, чем остальной:
не «обычно работает», а «доказуемо корректен в каждой точке, где процесс может
упасть».

Глава разбирает платёжный поток Molot снизу вверх: от представления суммы
в памяти до протокола оплаты, переживающего падение процесса между любыми
двумя строками. Сжатая версия — `docs/TEXTBOOK.md` §10 («Деньги: паранойя
как метод»); здесь — полный разбор с кодом.

## Проблема

Наивная реализация оплаты выглядит так:

```go
// АНТИПРИМЕР — не делайте так
func Pay(invoiceID string) error {
    tx := db.Begin()
    defer tx.Rollback()
    inv := tx.SelectForUpdate(invoiceID)
    ref, err := psp.Charge(inv.Total * 1.075) // float + внешний вызов под локом БД
    if err != nil {
        return err
    }
    inv.Status, inv.PSPRef = "paid", ref
    tx.Save(inv)
    return tx.Commit()
}
```

Здесь четыре независимые катастрофы:

1. **`float64` для сумм.** Копейки исчезают в округлениях, сравнения врут,
   погрешность накапливается.
2. **`Charge` внутри транзакции.** Таймаут PSP (30–60 секунд) держит
   `FOR UPDATE`-лок на строке. Все, кто хочет эту строку — воркер экспирации,
   повторный запрос клиента, — стоят в очереди. Несколько зависших платежей —
   и пул соединений исчерпан.
3. **Ретрай без idempotency key.** Клиент получил таймаут, нажал «оплатить»
   ещё раз — деньги списаны дважды.
4. **Нет ответа на вопрос «что с деньгами, если процесс упал ЗДЕСЬ?»** —
   между `Charge` и `Save`, между `Save` и ответом клиенту. В наивном коде
   ответ: «зависит от того, где упало», то есть деньги иногда теряются.

## Теория

### Почему float для денег — катастрофа

`float64` — это binary64 (IEEE 754): знак, 52 бита мантиссы, степень двойки.
Десятичная дробь `0.1` в двоичной системе — бесконечная периодическая, она
**непредставима точно** ни в каком количестве бит. Классика:

```
0.1 + 0.2 == 0.30000000000000004
```

Для физики это шум. Для денег — нет:

- **Погрешность накапливается.** Один платёж — ошибка в 16-м знаке. Миллион
  проводок в день, свёртка балансов, проценты от процентов — и расхождение
  выползает в копейки, потом в рубли. Сверка с PSP не сходится, и никто не
  может сказать почему.
- **Сравнения ненадёжны.** `if balance >= price` с накопленной погрешностью —
  случайный `false` на равных суммах; эпсилон-сравнение для денег — признание
  «мы не знаем точную сумму».
- **Округление непредсказуемо.** 7.5% от 999.99 во float зависит от порядка
  операций. Аргумент «так посчитал IEEE 754» в споре не работает.

Стандартное решение: **целые числа минорных единиц** (копейки, центы) + валюта
как отдельный тип. `int64` хватает на ~92 квадриллиона центов. Все операции —
целочисленные, округление — явное: в чью пользу копейка — решение бизнеса,
а не FPU.

### Идемпотентность платёжных операций

Сеть ненадёжна, и таймаут — это **неизвестность**, а не отказ: запрос мог
дойти и выполниться, а ответ — потеряться. Единственная стратегия для клиента —
повторить. Но «повторить списание» без защиты = списать дважды.

Индустриальный стандарт (Stripe, Adyen, ЮKassa): **idempotency key**. Провайдер
хранит результат первого выполнения по ключу и на повтор возвращает **тот же
результат**, не выполняя операцию заново — ретрай безопасен по построению.
Ключ должен быть **детерминированным от бизнес-операции**, а не случайным
per-request: «оплата инвойса X» — одна операция, сколько бы HTTP-запросов её
ни несло. Естественный кандидат — ID самого инвойса.

### Внешний вызов и транзакция БД несовместимы

Транзакция БД с `SELECT ... FOR UPDATE` — это эксклюзивный лок на строку.
Время удержания лока должно измеряться миллисекундами. Внешний HTTP-вызов —
секундами, а в плохой день — таймаутом в десятки секунд. Засунуть второе внутрь
первого — значит превратить деградацию PSP в деградацию своей БД: локи копятся,
пул соединений исчерпывается, ложится всё, а не только оплата.

Поэтому правило: **внешний вызов — строго до транзакции**. Цена правила —
появляется шов: между успешным `Charge` и коммитом записи мир может измениться
(инвойс истёк, процесс упал). Этот шов закрывают не локом, а **компенсацией**:
идемпотентным `Refund`, который безопасно вызывать и повторно, и «на всякий
случай».

### Crash seams: вопрос «что с деньгами?» для каждой строки

Шов (crash seam) — точка между двумя эффектами, где процесс может умереть,
оставив первый эффект без второго. В протоколе оплаты эффекта три: списание
в PSP, запись статуса в БД, ответ клиенту — значит, швов минимум три. Для
каждого нужен ответ на два вопроса: «что с деньгами сейчас?» и «что произойдёт
на ретрае?». Если хотя бы для одного шва ответ «зависит» — протокол не готов.

## Как в Molot

### Money: int64 минорных единиц + валюта как value object

`internal/billing/domain/invoice/money.go`:

```go
// Money is an amount in minor units of one currency. The zero value
// means "no money set" — never a valid amount of an unknown currency.
type Money struct {
	amount   int64
	currency Currency
}

// NewMoney builds a non-negative amount of minor units in currency.
func NewMoney(amountMinor int64, currency Currency) (Money, error) {
	if amountMinor < 0 {
		return Money{}, ErrNegativeAmount
	}
	if currency.IsZero() {
		return Money{}, ErrInvalidCurrency
	}
	return Money{amount: amountMinor, currency: currency}, nil
}

// Add returns m+other, guarding against mixing currencies.
func (m Money) Add(other Money) (Money, error) {
	if m.currency != other.currency {
		return Money{}, ErrCurrencyMismatch
	}
	return Money{amount: m.amount + other.amount, currency: m.currency}, nil
}

// MulBasisPoints returns bp/10000 of m, truncated toward zero —
// the platform never rounds a commission up in its own favor.
func (m Money) MulBasisPoints(bp int) Money {
	return Money{amount: m.amount * int64(bp) / 10_000, currency: m.currency}
}
```

Три решения, каждое — с «почему»:

- **Guard валют в `Add`.** Сложить евро с долларами нельзя физически — тип
  не даст. Без guard-а ошибка проявилась бы не на сложении, а через месяц
  на сверке, где её уже не отладить.
- **Проценты — целочисленно, в basis points.** Комиссия задаётся в сотых
  долях процента (`CommissionPolicy{basisPoints int}`,
  `internal/billing/domain/invoice/commission.go`), умножение — `int64`,
  деление — целочисленное. Никакого float на всём пути суммы.
- **Округление — явная политика, а не случайность.** Truncation toward zero:
  платформа никогда не округляет комиссию вверх в свою пользу. Это решение
  закреплено тестом (`internal/billing/domain/invoice/money_test.go`):

```go
	cases := []struct {
		amount int64
		bp     int
		want   int64
	}{
		{100_000, 1_000, 10_000}, // 10%
		{999, 1_000, 99},         // 99.9 → 99: never rounds up in the platform's favor
		{1, 1, 0},                // 0.0001 → 0
		{100, 10_000, 100},       // 100%
		{100, 0, 0},              // 0%
	}
```

Заметьте `{999, 1_000, 99}`: 9.99 центов комиссии превращаются в 9, а не в 10.
Копейка уходит клиенту — и это **задокументированное** поведение, на которое
можно сослаться в споре.

Ещё одна деталь: billing владеет **своей копией** `Money`, в auction — своя
(`internal/auction/domain/auction/money.go`). Дубль осознанный: контексты не
делят доменный код, иначе изменение типа в одном контексте каскадом ломает
другой (ARCHITECTURE.md §2.2).

### Контракт PSP: идемпотентный Charge и Refund

Порт объявлен на стороне потребителя — в `billing/app`, рядом с единственным
хендлером, которому он нужен. `internal/billing/app/command/pay_invoice.go`:

```go
// ChargeKey is the PSP idempotency key — by contract equal to the
// InvoiceID (§3.1): a repeated Charge with the same key never takes the
// money twice and returns the same reference.
type ChargeKey invoice.InvoiceID

// paymentGateway is the consumer-side PSP port (§3.1). Contract:
//   - Charge is idempotent per key: a retry returns the original
//     reference without a second debit. A network failure surfaces as
//     invoice.ErrPSPUnavailable, a refusal as invoice.ErrPaymentDeclined.
//   - Refund is idempotent and a no-op when no charge exists for the
//     key — safe to call as crash-seam insurance.
type paymentGateway interface {
	Charge(ctx context.Context, key ChargeKey, amount invoice.Money) (invoice.PaymentReference, error)
	Refund(ctx context.Context, key ChargeKey) error
}
```

Контракт — это не подпись методов, а **семантика в комментарии**: повторный
`Charge` по тому же ключу не списывает второй раз и возвращает тот же reference;
`Refund` идемпотентен и no-op без charge-а. На этих двух свойствах держится
весь протокол ниже — без них ни один шов не закрывается.

`ChargeKey = InvoiceID` — детерминированный ключ от бизнес-операции. Сам
`InvoiceID` тоже детерминирован (uuidv5 от `auctionID + ":" + attempt`,
см. `invoice.go`), так что цепочка идемпотентности тянется от саги до PSP.

Фейковый адаптер честно реализует контракт —
`internal/billing/adapters/psp_fake.go`:

```go
func (p *FakePSP) Charge(_ context.Context, key command.ChargeKey, _ invoice.Money) (invoice.PaymentReference, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	k := key.String()
	if ref, ok := p.charges[k]; ok {
		return ref, nil // idempotent: same key → same reference, no double debit
	}
	// ...
}

func (p *FakePSP) Refund(_ context.Context, key command.ChargeKey) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	k := key.String()
	if _, ok := p.charges[k]; !ok {
		return nil // no charge for this key — no-op by contract
	}
	p.refunded[k] = true // idempotent: repeating a refund changes nothing
	return nil
}
```

Режим управляется `PSP_MODE=success|decline|flaky`; `flaky` — первый `Charge`
по ключу падает сетевой ошибкой, повтор успешен — ровно то, что нужно
component-тестам ретраев. Контракт фейка сам закреплён тестами
(`internal/billing/adapters/psp_fake_test.go`:
`TestFakePSPSuccessChargeIsIdempotentByKey`, `TestFakePSPRefund`): фейк,
который врёт о провайдере, превращает тесты протокола в проверку выдумки.

### Протокол PayInvoice: Charge строго до транзакции

Полный хендлер — `internal/billing/app/command/pay_invoice.go`. Шаг 1,
pre-check без лока:

```go
	switch inv.Status() {
	case invoice.StatusPaid:
		// Retry after a successful payment: idempotent success.
		// Critically, Refund is NOT called — the money stays charged.
		return nil
	case invoice.StatusExpired, invoice.StatusVoided:
		// Insurance for the crash seam between a lost race and its
		// Refund: the retry lands here and repeats the idempotent
		// Refund (a no-op when nothing was charged).
		if err := h.psp.Refund(ctx, key); err != nil {
			return errs.NewUnavailableError("psp-unavailable").WithCause(err)
		}
		return errs.NewConflictError("invoice-no-longer-payable")
	case invoice.StatusPending:
		// Proceed to charge.
	default:
		panic("billing: unknown invoice status " + inv.Status().String())
	}
```

Два неочевидных решения в десяти строках:

- **`Paid` → `nil` (204), и Refund НЕ вызывается.** Это самая дорогая ветка
  протокола, если её перепутать. Ретрай после успешной оплаты — штатная
  ситуация (ответ потерялся в сети). Вызвать здесь Refund «для симметрии» —
  значит вернуть клиенту деньги за оплаченный товар: платформа теряет деньги
  на каждом сетевом сбое.
- **`Expired | Voided` → страховочный Refund, потом 409.** Зачем Refund, если
  мы ещё ничего не списывали? Затем, что, возможно, списывали — в **прошлой**
  попытке, которая упала между обнаружением гонки и компенсацией (см. швы ниже).
  Refund идемпотентен и no-op без charge-а — вызывать его здесь безопасно
  всегда и необходимо иногда.

Шаги 2–4 — списание и интерпретация гонки:

```go
	// Step 2 — Charge, strictly before the transaction. The client
	// retries 502s; the repeated Charge is idempotent by key.
	ref, err := h.psp.Charge(ctx, key, inv.Total())
	switch {
	case errors.Is(err, invoice.ErrPaymentDeclined):
		return errs.NewConflictError("payment-declined").WithCause(err)
	case err != nil:
		return errs.NewUnavailableError("psp-unavailable").WithCause(err)
	}

	// Step 3 — MarkPaid under SELECT ... FOR UPDATE; the commit
	// publishes InvoicePaidV1 through the outbox.
	err = h.repo.Update(ctx, cmd.InvoiceID, cmd.Payer,
		func(_ context.Context, inv *invoice.Invoice) (*invoice.Invoice, error) {
			if err := inv.MarkPaid(ref, h.clock.Now()); err != nil {
				return nil, err
			}
			return inv, nil
		})

	// Step 4 — interpret the race outcome.
	switch {
	case err == nil:
		return nil
	case errors.Is(err, invoice.ErrInvoiceAlreadyPaid):
		// Concurrent duplicate of the same payment (same idempotency
		// key): success, and no Refund.
		return nil
	case errors.Is(err, invoice.ErrInvoiceExpired), errors.Is(err, invoice.ErrInvoiceVoided):
		// Race lost: the invoice expired or was voided between Charge
		// and the row lock — compensate the charge.
		if refundErr := h.psp.Refund(ctx, key); refundErr != nil {
			return errs.NewUnavailableError("psp-unavailable").WithCause(refundErr)
		}
		return errs.NewConflictError("invoice-no-longer-payable").WithCause(err)
	}
```

Обратите внимание на классификацию ошибок `Charge`: сетевая ошибка → 502
(`Unavailable`, клиент **должен** ретраить — повтор бесплатен благодаря ключу),
отказ PSP → 409 (`Conflict`, ретраить бессмысленно — карта отклонена). Перепутать
коды — значит либо заставить клиента долбить отклонённую карту, либо отучить
его ретраить восстановимые сбои.

### Гонка charged-but-expired

Раз `Charge` вынесен из транзакции, между ним и `FOR UPDATE`-локом есть окно.
В этом окне воркер экспирации может успеть перевести инвойс в `Expired` (или
сага — в `Voided`). Получается состояние «деньги списаны, а счёт уже не
оплачиваемый» — то самое charged-but-expired.

Протокол не пытается окно устранить (это вернуло бы внешний вызов под лок) —
он его **детектирует и компенсирует**. `MarkPaid` внутри `updateFn` бьётся
о guard агрегата, транзакция откатывается, хендлер по сентинелу понимает,
что гонка проиграна, и возвращает деньги идемпотентным `Refund`. Тест ветки —
`internal/billing/app/command/pay_invoice_test.go`:

```go
	t.Run("race lost to expiry: compensating refund then conflict", func(t *testing.T) {
		// ...
		repo := &repoSpy{inv: inv, updateErr: invoice.ErrInvoiceExpired}

		handleErr := h.Handle(context.Background(), payCmd(inv))

		assert.True(t, errors.Is(handleErr, errs.NewConflictError("invoice-no-longer-payable")))
		assert.Len(t, psp.charges, 1, "charge happened before the lock")
		require.Len(t, psp.refunds, 1, "the lost race must be compensated")
	})
```

А `ErrInvoiceAlreadyPaid` на том же шве — **не** проигранная гонка, а
конкурентный дубль того же платежа (два запроса клиента наперегонки, один и
тот же idempotency key, деньги списаны один раз). Ответ — 204 и никакого
Refund-а. Различать эти два исхода и позволяет guard-таблица агрегата: домен
возвращает точную причину, хендлер интерпретирует.

### Crash seams: что происходит на ретрае

Теперь главное. Процесс может умереть между любыми двумя эффектами протокола.
Для каждого шва — состояние, поведение ретрая и тест, который это охраняет:

| Шов (упали здесь) | Состояние | Ретрай клиента | Тест |
|---|---|---|---|
| После `Charge`, до `MarkPaid` | Деньги списаны, инвойс `Pending` | Pre-check: `Pending` → повторный `Charge` (no-op по ключу, тот же ref) → `MarkPaid`. Оплата завершается | `TestPaymentRefund_Flaky` (component); idempotency — `TestFakePSPSuccessChargeIsIdempotentByKey` |
| Между обнаружением гонки и `Refund` | Деньги списаны, инвойс `Expired`/`Voided` | Pre-check: `Expired`/`Voided` → **страховочный Refund повторяется** → 409 | `"expired invoice: insurance refund then conflict"` |
| После commit `MarkPaid`, до ответа | Деньги списаны, инвойс `Paid` | Pre-check: `Paid` → 204, Refund не вызывается | `"retry of a paid invoice is 204 and never refunds"` |

Ни в одном шве деньги не теряются и не задваиваются — **при условии, что клиент
ретраит**. Протокол не магический, он перекладывает восстановление на ретрай,
но гарантирует, что ретрай всегда безопасен и всегда сходится к правильному
исходу. Если Refund сам упал сетью — хендлер отвечает 502, и следующий ретрай
повторит его из pre-check-ветки
(`"refund failure after a lost race is 502 (retry repeats the refund)"`).

Юнит-тест самого дорогого шва (`internal/billing/app/command/pay_invoice_test.go`):

```go
	t.Run("retry of a paid invoice is 204 and never refunds", func(t *testing.T) {
		// ... inv уже Paid ...
		require.NoError(t, h.Handle(context.Background(), payCmd(inv)))

		assert.Empty(t, psp.charges, "no second charge")
		assert.Empty(t, psp.refunds, "refund after a successful payment would lose the platform's money")
	})
```

Первый шов прогоняется и на живом приложении —
`tests/component/payment_refund_test.go`, `PSP_MODE=flaky`:

```go
	// --- first PayInvoice: PSP_MODE=flaky → 502 psp-unavailable --------------
	status1 := c.PayInvoiceExpect(t, invoiceID, winnerToken)
	assert.Equal(t, http.StatusBadGateway, status1,
		"first payment attempt must return 502 (psp-unavailable)")

	// --- second PayInvoice (retry): PSP idempotency key → same charge ref, success
	c.PayInvoice(t, invoiceID, winnerToken) // asserts 204
```

`TestPayInvoiceProtocol` в сумме покрывает каждую ветку протокола — 404,
анти-enumeration, 204-идемпотентность, обе страховочные ветки, decline, сетевой
сбой, happy path, обе проигранные гонки, дубль платежа и отказ Refund-а. Это
не перфекционизм: каждая ветка — это сценарий с деньгами, и у каждого сценария
должен быть тест, который сломается, если кто-то «упростит» хендлер.

### Гонка Paid vs Expired: агрегат как арбитр

Последняя гонка — не про PSP, а про два писателя в БД: хендлер оплаты и воркер
экспирации одновременно тянутся к одному инвойсу. Арбитром выступает агрегат
под `FOR UPDATE`: оба писателя сериализуются на строке, и тот, кто пришёл
вторым, бьётся о guard. `internal/billing/domain/invoice/invoice.go`:

```go
// MarkPaid transitions Pending → Paid. The deadline is deliberately NOT
// enforced here: expiry belongs to the worker, so a payment that lands
// before the Expire transaction commits stays valid (grace = poll
// interval, §2.2 guard table).
func (i *Invoice) MarkPaid(ref PaymentReference, now time.Time) error {
	if ref.IsZero() {
		return ErrEmptyPaymentReference
	}
	switch i.status {
	case StatusPending:
		i.status = StatusPaid
		i.pspRef = ref
		i.record(InvoicePaid{ /* ... */ })
		return nil
	case StatusPaid:
		return ErrInvoiceAlreadyPaid
	case StatusExpired:
		return ErrInvoiceExpired
	case StatusVoided:
		return ErrInvoiceVoided
	default:
		panic("invoice: unknown status " + i.status.s)
	}
}
```

Два следствия:

- **Проигравший не публикует событие.** `record(InvoicePaid{...})` выполняется
  только внутри успешного перехода; на guard-ошибке транзакция откатывается,
  и outbox-строка не пишется. Сага физически не может увидеть и `InvoicePaidV1`,
  и `InvoiceExpiredV1` по одному инвойсу — выигрывает ровно один исход.
  Полная guard-таблица переходов (`MarkPaid`/`Expire`/`Void` × 4 статуса) —
  ARCHITECTURE.md §2.2.
- **Дедлайн в `MarkPaid` сознательно не проверяется.** Экспирация — забота
  воркера. Платёж, успевший до коммита `Expire`-транзакции, остаётся валидным,
  даже если `dueAt` уже в прошлом: grace-период равен интервалу поллинга
  воркера. Альтернатива — проверять `now < dueAt` в `MarkPaid` — создала бы
  обратную гонку: платёж, прошедший Charge за миллисекунду до дедлайна,
  отбивался бы на записи, хотя деньги уже списаны.

Гонка целиком, на живом приложении с воркером и сагой —
`tests/component/payment_refund_test.go`, `TestPaymentRefund_ExpiredRace`:
короткий `PAYMENT_TERM` (2 s), конкурентная оплата против таймера экспирации;
исход — 204 либо 409, и сага доходит до терминального состояния в обоих случаях.

## Трейдоффы

Каждое решение выше что-то стоит. Честный список:

- **Синхронный Charge — учебное упрощение.** Реальные PSP (Stripe, Adyen,
  ЮKassa) подтверждают оплату асинхронно: ответ `Charge` — это «принято»,
  финальный статус приходит вебхуком с HMAC-подписью. Протокол Molot корректен
  для синхронной модели; перенос на вебхуки — ROADMAP.md §6.2.1, и он добавит
  состояние `processing` и reconcile-логику.
- **Truncation toward zero — простая, но не лучшая политика округления.**
  Для одной комиссии — отлично (предсказуемо, не в пользу платформы). Но при
  разбиении суммы на части truncation теряет копейки: сумма частей может не
  сойтись с целым. Банковское округление и property-тест «сумма частей ==
  целому» — ROADMAP.md §6.1.2.
- **Refund как компенсация vs auth/capture.** Charged-but-expired можно было
  закрыть иначе: hold (авторизация) при начале оплаты → capture при коммите.
  Тогда «гонка проиграна» — это отмена hold-а, денег клиент вообще не видит
  списанными. Это правильнее для UX, но требует двухфазной модели PSP —
  ROADMAP.md §6.2.4.
- **`ChargeKey = InvoiceID` связывает идемпотентность с жизненным циклом
  инвойса.** Удобно: ключ детерминирован, ничего не надо хранить. Ограничение:
  один инвойс — максимум одно списание за всю жизнь. Частичные оплаты или
  повторная оплата после refund-а потребуют составного ключа.
- **`MulBasisPoints` не защищён от переполнения.** `amount * int64(bp)` —
  при максимальных 10 000 bp переполнение наступает на суммах свыше ~922
  триллионов минорных единиц. Для аукциона это не риск, но при копировании
  кода в систему с другими порядками сумм проверку придётся добавить.
- **Своя копия `Money` в каждом контексте** — это дублирование. Плата за
  независимость контекстов: два файла, два набора тестов. Альтернатива —
  общий пакет `money` — дешевле сегодня и дороже при первом расхождении
  требований (auction не нужен `MulBasisPoints`, billing не нужны ставки).

### Граница текущей реализации: деньги проходят, но не хранятся

Важно проговорить честно. Всё выше гарантирует, что деньги **проходят сквозь**
систему корректно: нет двойных списаний, потерянных событий и lost updates.
Но система деньги **не хранит**: нет счетов, балансов, проводок, сверки,
выплат продавцам. Состояние денег — это поле `status` инвойса плюс память PSP.

Что это значит на практике: вопрос «сколько платформа должна продавцу X»
сегодня не имеет ответа в данных — его можно вычислить только косвенно, по
выборке оплаченных инвойсов. Вопрос «сходится ли наш учёт с отчётом PSP» не
имеет ответа вообще — нечего сверять.

Следующий шаг — **ledger двойной записи**: append-only журнал проводок
(«дебет покупателя / кредит продавца / кредит комиссии»), инвариант «сумма
проводок транзакции = 0», баланс как свёртка истории, исправления только
сторно-проводками. Деньги становятся фактами, а не полями статуса; любой
баланс воспроизводим из истории. План и объёмы — ROADMAP.md §6 (особенно
§6.1.1 ledger, §6.1.4 балансы и payouts, §6.2.2 reconciliation).

## Типичные ошибки

1. **`float64` для сумм** — включая «только для отображения», откуда он
   неизбежно протекает в логику. Конвертация в минорные единицы — на границе
   системы, один раз.
2. **Charge внутри транзакции БД.** Таймаут провайдера держит лок; деградация
   PSP становится деградацией БД. Внешний вызов — до транзакции, шов —
   компенсацией.
3. **Ретрай без idempotency key** — или со случайным ключом per-request, что
   то же самое: каждый ретрай выглядит новой операцией.
4. **Refund на ветке Paid** «для симметрии». Возврат денег за успешно
   оплаченный счёт на каждом потерянном ответе. Ветка `Paid` → 204 и ничего
   больше — самая важная строка протокола.
5. **Сетевой сбой и decline под одним кодом ответа.** 502 значит «ретрай
   безопасен и нужен», 409 — «ретрай бессмыслен». Смешать — клиент либо
   долбит мёртвую карту, либо не ретраит восстановимое.
6. **Компенсация без страховочной ветки.** Refund после проигранной гонки —
   тоже эффект, и до него тоже можно упасть. Если ретрай не повторяет
   компенсацию (ветка `Expired|Voided` → Refund в pre-check), деньги клиента
   зависают навсегда.
7. **Проверка дедлайна в двух местах.** Дедлайн enforce-ит либо запись оплаты,
   либо воркер экспирации — не оба: иначе появляется окно, где платёж с уже
   списанными деньгами отбивается на записи.
8. **Публикация события до/вне транзакции состояния.** Проигравший гонку
   публикует своё событие — и сага видит оба исхода. Событие — только через
   outbox, в той же транзакции, что и переход.
9. **Ветки протокола без тестов.** Каждая ветка — денежный сценарий. Ветка
   без теста — это ветка, которую следующий рефакторинг молча удалит.

## Чек-лист

Перед тем как выпускать платёжный код:

- [ ] Суммы — целые минорные единицы; float не встречается на пути денег
      ни в одной точке, включая JSON-маппинг.
- [ ] Валюта — часть типа суммы; операции над разными валютами не компилируются
      или падают guard-ом.
- [ ] Политика округления задокументирована («в чью пользу копейка») и закреплена
      тестом с граничными значениями.
- [ ] У каждой денежной операции внешнего провайдера есть idempotency key,
      детерминированный от бизнес-операции.
- [ ] Внешние вызовы — вне транзакций БД; время удержания локов не зависит
      от латентности провайдера.
- [ ] Для каждого шва «упали здесь» написан ответ: что с деньгами, что сделает
      ретрай — и на каждый ответ есть тест.
- [ ] Ретрай после успеха — идемпотентный успех без побочных эффектов
      (и точно без Refund-а).
- [ ] Компенсация идемпотентна и повторяется на ретрае, если сама упала.
- [ ] Retryable и terminal ошибки различимы по коду ответа.
- [ ] Конкурентные писатели сериализуются на агрегате; проигравший не публикует
      событие.
- [ ] Известно и записано, чего система пока НЕ умеет (ledger, сверка, выплаты) —
      и это решение, а не пробел.

## Ссылки

- `docs/ARCHITECTURE.md` §3.1 — протокол PayInvoice: шаги, коды ответов,
  crash-seams, контракт PSP.
- `docs/ARCHITECTURE.md` §2.2 — агрегат Invoice: guard-таблица переходов,
  анти-enumeration, sentinel-ошибки.
- `docs/ROADMAP.md` раздел 6 — что дальше: ledger двойной записи (§6.1.1),
  политика округления (§6.1.2), балансы и выплаты (§6.1.4), вебхуки PSP
  (§6.2.1), reconciliation (§6.2.2), auth/capture (§6.2.4).
- `docs/TEXTBOOK.md` §10 — сжатая версия этой главы и золотое правило 10.
- Код: `internal/billing/domain/invoice/money.go`, `commission.go`,
  `invoice.go`, `errors.go`; `internal/billing/app/command/pay_invoice.go`;
  `internal/billing/adapters/psp_fake.go`.
- Тесты: `internal/billing/domain/invoice/money_test.go`;
  `internal/billing/app/command/pay_invoice_test.go` (`TestPayInvoiceProtocol`);
  `internal/billing/adapters/psp_fake_test.go`;
  `tests/component/payment_refund_test.go`.
