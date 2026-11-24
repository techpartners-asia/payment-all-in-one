# payment-all-in-one

One Go client for every Mongolian payment provider covered by the
techpartners-asia SDKs. You configure the providers you use, then create, check,
cancel and refund invoices through a single interface. Every result has the same
shape whichever provider handled it.

```
go get github.com/techpartners-asia/payment-all-in-one
```

> **Build note.** Until four upstream SDKs are updated, this module does not
> compile on its own. Details are in [Known upstream issues](#known-upstream-issues).

## Usage

```go
import (
	payment "github.com/techpartners-asia/payment-all-in-one"
	"github.com/techpartners-asia/payment-all-in-one/providers/qpay"
	"github.com/techpartners-asia/payment-all-in-one/providers/socialpay"
)

client, err := payment.New(payment.Config{
	QPay: &qpay.Config{
		Username:    os.Getenv("QPAY_USERNAME"),
		Password:    os.Getenv("QPAY_PASSWORD"),
		Endpoint:    os.Getenv("QPAY_ENDPOINT"),
		InvoiceCode: os.Getenv("QPAY_INVOICE_CODE"),
		CallbackURL: "https://shop.example/callback/qpay",
	},
	SocialPay: &socialpay.Config{ /* ... */ },
})
if err != nil {
	log.Fatal(err) // a provider config is missing required fields
}

inv, err := client.CreateInvoice(ctx, payment.CreateInvoiceInput{
	Provider:    payment.ProviderQPay,
	OrderID:     "order-123",
	Amount:      15000,
	Description: "Order #123",
})
// inv.QRText, inv.QRImage, inv.PaymentURL, inv.Deeplinks

st, err := client.CheckInvoice(ctx, payment.CheckInvoiceInput{
	Provider:  inv.Provider,
	InvoiceID: inv.InvoiceID,
	OrderID:   inv.OrderID,
	Amount:    inv.Amount,
})
if st.Status == payment.StatusPaid { /* fulfil */ }
```

### One input for every provider, validated before any network call

`CreateInvoiceInput` is the same for all 15 providers. Each provider declares
its own `InvoiceRules`:

- which fields it requires
- which fields it needs at least one of
- whether it accepts only whole tögrög
- every `Extra` key it reads, with the values it accepts

`CreateInvoice` enforces these rules before any network call. The adapters
enforce them too, so calling a provider package directly is also safe.

- **All problems at once.** A wrong input returns every problem in a single
  `ErrInvalidInput` error, for example
  `storepay: OrderID is required; storepay: Phone is required`.
- **Typos are rejected.** An unknown `Extra` key, such as `card_no` instead of
  `card_number`, is an error instead of being silently ignored.
- **No silent rounding.** Providers that need whole tögrög reject `1500.50`.
  The others pass decimals through unchanged.

```go
rules, _ := client.InvoiceRules(payment.ProviderStorePay)
// rules.Required == [Phone]: ask the customer for a phone number.

if err := client.ValidateInvoice(in); err != nil {
	// show err to the user; nothing was sent to the provider
}
```

| Provider | Needs beyond OrderID + Amount | Whole tögrög |
|---|---|---|
| StorePay | `Phone` | |
| UPoint | `Phone` or `Extra["card_number"]` | |
| DigiPay | `Extra["bearer_token"]` | |
| Balc | `CustomerID` (numeric) | yes |
| QPay, Golomt | `CallbackURL`, unless set in config | QPay only |
| Bonum | `ReturnURL` for `Extra["flow"]="checkout"`, unless set in config | |
| Simple, MongolChat | none | yes |
| Tino, SocialPay, MonPay, Pocket, HiPay, ArdPay | none | |

`rules_test.go` keeps these rules honest. It fails in three cases:

- an adapter rejects input that its declared rules accept
- an adapter reads an `Extra` key it does not declare
- an adapter declares a key it never reads

A requirement that depends on another field, such as Bonum's `ReturnURL`
being needed only when `flow` is `checkout`, goes in `InvoiceRules.Check`.
`ValidateInvoice` runs those checks too.

A provider is enabled when its config pointer is set. `New` validates every
enabled config and fails at startup if any required field is empty.

Store `InvoiceID`, `OrderID` and `Amount` from the invoice. `CheckInvoice`,
`CancelInvoice` and `Refund` take them back, and some providers sign requests
over the amount.

`CancelInvoice` and `Refund` return `payment.ErrUnsupported` for providers
without that operation. Errors wrap one of these sentinels:

- `ErrNotConfigured`
- `ErrInvalidInput`
- `ErrInvalidConfig`
- `ErrUnsupported`
- `*ProviderError`, for SDK and API failures, which carries the provider and the operation

An unknown provider status always maps to `StatusPending`, never to paid.

Always pass the expected `Amount` to `CheckInvoice`. If a provider reports
paying less than that amount, the client downgrades `StatusPaid` to
`StatusPending`. This stops someone from settling an expensive order with the
id of a cheaper invoice. StorePay and MongolChat report no paid
amount, so for those providers you must make sure the `InvoiceID` you check
belongs to the order.

You can add a custom provider or a test double with `client.Register(gw)`,
where `gw` implements `payment.Gateway`.

## Providers

| Provider | Package | SDK | Cancel | Refund | Required config |
|---|---|---|---|---|---|
| Tino | `providers/tino` | `tino-go` | yes | | AuthURL, BaseURL, Username, Password, WalletID |
| QPay | `providers/qpay` | `qpay-go/qpay_v2` | yes | full only | Username, Password, Endpoint, InvoiceCode |
| Golomt e-commerce | `providers/golomt` | `golomt-api-go/ecommerce` | | full only | Secret, BearerToken |
| SocialPay | `providers/socialpay` | `golomt-api-go/socialpay` | yes | full only | Terminal, Secret, Endpoint |
| MonPay | `providers/monpay` | `monpay-go/monpay` | | | Endpoint, Username, AccountID |
| Pocket | `providers/pocket` | `pocket-go` | | | ClientID, ClientSecret, TerminalID |
| Bonum | `providers/bonum` | `bonum-go` | | | AppSecret, TerminalID |
| HiPay | `providers/hipay` | `hipay-go/hipay` | yes | full, same day | Endpoint, Token, EntityID |
| DigiPay | `providers/digipay` | `digipay-go` | | | BaseURL, ClientID, ClientSecret, MerchantID, MerchantName, TerminalID |
| ArdPay | `providers/ardpay` | `ardpay-go` | yes | | BaseURL, MerchantID, PosNo, APIKey |
| MongolChat | `providers/mongolchat` | `mongolchat-go/mongolchat` | | yes | Endpoint, APIKey, WorkerKey |
| UPoint | `providers/upoint` | `upoint-go` | | yes | Endpoint, Token, TerminalID |
| Simple | `providers/simple` | `simple-go` | | | Username, Password, BaseURL |
| StorePay | `providers/storepay` | `storepay-go` | | | AppUsername, AppPassword, Username, Password, AuthURL, BaseURL, StoreID |
| Balc | `providers/balc` | `balc-api-go` | | | Endpoint, Token |

Each package's doc comment documents the provider in full. That covers optional
config, how `InvoiceID` is chosen, the status mapping, and its `Extra` keys.

### Provider-specific behaviour worth knowing

- **Tino, QPay, Pocket.** The adapter caches the access token and refreshes it
  30 seconds before it expires. Concurrent callers share one login. If the
  provider rejects a token, the adapter logs in again and retries once.
  QPay renews through `/auth/refresh` while its refresh token is live and
  logs in only when it is not. Tino and Pocket have no refresh endpoint, so
  they log in again.
- **Simple, StorePay.** The SDK caches its token per client and fixes the
  callback URL per client, so the adapter keeps one client per callback URL
  (up to 16). A repeated `CallbackURL` override reuses that client's token
  instead of logging in again.
- **Golomt.** `Extra` keys:
  - `return_type`: `GET`, `POST` or `MOBILE`
  - `lang`: `MN` or `EN`
  - `payment_method`: `payment` or `socialpay`
  - `get_token` and `social_deeplink`: booleans

  Checking an unpaid invoice returns a `*ProviderError`. The SDK raises an error
  for every code except `000`.
- **SocialPay.** If `Phone` is set, the customer gets a phone invoice; otherwise
  you get a QR invoice. `CheckInvoice`, `CancelInvoice` and `Refund` require
  `Amount`. Checking an unpaid invoice returns a `*ProviderError`.
- **Bonum.** `Extra["flow"]` selects the payment flow:
  - `qr` (the default) returns a QR code and bank deeplinks.
  - `checkout` returns a hosted page and needs a `ReturnURL`.

  `CheckInvoice` returns `ErrUnsupported`. Bonum's webhook is the source of
  truth, and Bonum blocks the status endpoint in production. Use
  `bonum.ParseWebhook` from `bonum-go`.
- **DigiPay.** Invoices are created on behalf of a superapp user. Pass the
  user's token as `Extra["bearer_token"]`.
- **UPoint.** This is a loyalty-points payment, so `CreateInvoice` spends the
  points immediately and returns `StatusPaid`. Pass `Extra["card_number"]` or
  `Phone`. To reverse it, call `Refund` with Amount 0 to cancel the whole
  transaction, or with an amount and a `TransactionID` to return part of it.
- **StorePay.** This is buy-now-pay-later and needs `Phone`. A check only says
  whether the loan was confirmed, so `PaidAmount` stays 0.
- **Balc.** This is credit. `CreateInvoice` checks the customer's limit, then
  grants the loan synchronously, so it returns `StatusPaid`. `CustomerID` must
  be the numeric BALC customer id. `CheckInvoice` returns `ErrUnsupported`.
- **Simple.** `ExpiresIn` defaults to 20 minutes. Amounts must be whole
  numbers.

## Known upstream issues

`bonum-go` requires `resty.dev/v3 v3.0.0-rc.3`. These four SDKs still call
Response methods that rc.3 renamed:

| SDK | Version | Change needed |
|---|---|---|
| `tino-go` | v1.0.29 | `res.IsSuccess()` → `res.IsStatusSuccess()` |
| `ardpay-go` | v0.0.8 | `res.IsError()` → `res.IsStatusFailure()` |
| `monpay-go` | v1.2.0 | `res.IsError()` → `res.IsStatusFailure()` |
| `hipay-go` | v1.0.2 | `res.IsError()` → `res.IsStatusFailure()` |

Go allows only one resty version per build, so this module compiles only once
those four are released with the rename. Until then, build against locally
patched copies through a `go.work` file:

```
go 1.26.2

use (
	.
	../patched/tino-go
	../patched/ardpay-go
	../patched/monpay-go
	../patched/hipay-go
)
```

The adapters work around other SDK defects, which are documented in each
package. They include nil-pointer panics on network failure (UPoint,
MongolChat, Simple, StorePay, Balc), JSON decode errors that are silently
ignored, and no request timeouts. `mongolchat-go` also pulls in
`github.com/dgrijalva/jwt-go`, which has CVE-2020-26160.

## Development

```
GOWORK=/path/to/go.work go test -race ./...
```

The tests run against `httptest` servers that return each provider's response
shapes. No test touches the network.
