# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go library (`github.com/techpartners-asia/payment-all-in-one`) that puts one `payment.Client` in front of 15 Mongolian payment-provider SDKs (all `github.com/techpartners-asia/*-go`). There is no binary, no Makefile, and no lint config. Use `go vet` and `gofmt`.

## Build and test

The module does **not** compile as a whole against the published SDKs. `bonum-go` pins `resty.dev/v3 v3.0.0-rc.3`, and `tino-go`, `ardpay-go`, `monpay-go` and `hipay-go` still call resty methods that rc.3 renamed (`IsError`/`IsSuccess`). `go build ./...`, `go vet ./...` and the root package tests fail until those SDKs are patched. To work around it, point `GOWORK` at a `go.work` that `use`s locally patched copies of the four SDKs. The README "Known upstream issues" section has the template.

```sh
GOWORK=/path/to/go.work go test -race ./...           # full suite (needs patched SDKs)
go test ./providers/qpay ./providers/storepay          # adapters whose SDK builds work without go.work
go test ./providers/qpay -run TestCheckInvoicePaid -v  # single test
```

Without `go.work`, anything that imports tino, ardpay, monpay or hipay fails to build. That includes the root package (`payment_test.go`, `rules_test.go`), because `payment.go` imports every adapter. No test touches the network. Each adapter test runs an `httptest` server that returns that provider's response shapes.

## Architecture

Three layers:

- **`core/`** holds the provider-neutral contract: the `Provider`/`Status` enums, the input and result structs, the `Gateway` interface, the optional `Canceler`/`Refunder` interfaces, the sentinel errors, `ProviderError` + `core.Wrap`, `RequireConfig`, and `InvoiceRules` with its `Validate`. Adapters import only `core`, never the root package.
- **`providers/<name>/`** has one adapter per provider and always the same three files:
  - `<name>.go`: the `Config` struct, `New(Config) (*Adapter, error)` (calls `core.RequireConfig`), and the `Gateway` methods. The package doc comment is the authoritative per-provider documentation: how `InvoiceID` is chosen, the status mapping, `Extra` keys, and SDK quirks.
  - `rules.go`: `func (a *Adapter) InvoiceRules() core.InvoiceRules`. It may depend on config, for example requiring `CallbackURL` per call only when the config has none.
  - `<name>_test.go`: adapter tests against a fake HTTP server.
- **Root `payment.go`** re-exports the `core` types as aliases, defines `Config`, which has one pointer field per provider (nil means disabled), and `Client`, which routes by `Provider`. `New` builds every non-nil config via the generic `build` helper and fails if any config is invalid. `Register` adds custom or fake gateways. Cancel and refund are optional and are discovered by type assertion to `core.Canceler`/`core.Refunder`. Without them the call returns `ErrUnsupported`.

### Invariants to preserve

- **Validation runs twice on purpose.** `Client.CreateInvoice` calls `InvoiceRules().Validate` before routing, and each adapter's `CreateInvoice` calls it again, so calling a provider package directly is just as safe. `Validate` collects every problem and joins them with `errors.Join`. Each one wraps `ErrInvalidInput`. Unknown `Extra` keys are errors. `WholeAmount` providers reject fractional amounts and never round them.
- **`rules_test.go` enforces that declared rules match what each adapter does.** `TestDeclaredExtraKeysMatchSource` regex-scans `providers/<p>/<p>.go` for `Extra["key"]` / `extra["key"]` / `(Extra|extra), "key"` and fails on keys that are read but not declared, or declared but never read. Keep `Extra` reads in that literal form, in the main adapter file. `TestRulesMatchAdapterBehavior` builds the minimal input the rules allow and asserts it reaches the provider. Requirements that depend on other fields belong in `InvoiceRules.Check`, wrapping `ErrInvalidInput`. Bonum's checkout `ReturnURL` is the example.
- **Status safety.** An unknown provider status must map to `StatusPending`, never to paid. `Client.CheckInvoice` applies `guardUnderpayment`: it downgrades paid to pending when the reported `PaidAmount` is below the caller's expected `Amount`. Adapters must therefore fill in `PaidAmount` whenever the provider reports it.
- **Errors.** Wrap SDK and API failures with `core.Wrap(provider, "op_name", err)` to produce a `*ProviderError`. Return the sentinels (`ErrInvalidInput`, `ErrInvalidConfig`, `ErrUnsupported`, `ErrNotConfigured`) wrapped with `%w`.

### Adapters defend against SDK defects

Several upstream SDKs are buggy, and the adapters work around them locally instead of trusting them. Look at an existing adapter before writing a new one:

- `recoverSDK` (storepay, simple, balc, and similar) turns SDK nil-pointer panics on network failure into errors.
- A `sync.Mutex` serializes SDK calls where the SDK caches tokens in unguarded fields (storepay).
- Tino, QPay and Pocket cache their access token, refresh it 30s before expiry, share a single login across concurrent callers, and re-login and retry once when a token is rejected.
- Where the SDK fixes the callback URL at construction, a per-call `CallbackURL` override builds a fresh SDK client.

## Adding a provider

1. Add the constant to `core.Provider`.
2. Create `providers/<name>/{<name>.go,rules.go,<name>_test.go}` following the layout above.
3. Wire it into `payment.go`: the alias constant, a `Config` field, and a `build(...)` entry in `New`.
4. Add it to the all-providers client in `rules_test.go`. It asserts exactly 15 configured providers.
5. Update the README tables.
