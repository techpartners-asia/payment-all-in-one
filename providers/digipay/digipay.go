// Package digipay adapts the DigiPay superapp invoice API
// (github.com/techpartners-asia/digipay-go) to core.Gateway.
//
// DigiPay creates invoices on behalf of a signed-in superapp user, so
// CreateInvoice needs that user's OAuth access token in
// Extra["bearer_token"]. Obtain it with the SDK's ExchangeAuthorizationCode
// or RefreshUserAccess. CheckInvoice uses the merchant's client credentials
// and needs only the InvoiceID.
//
// Extra keys:
//   - bearer_token (required on CreateInvoice): end-user superapp access token.
//
// Optional operations: none; the SDK exposes no cancel or refund call.
package digipay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	sdk "github.com/techpartners-asia/digipay-go"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

type Config struct {
	BaseURL      string
	ClientID     string
	ClientSecret string
	MerchantID   string
	MerchantName string
	TerminalID   string
	// CallbackURL is sent as the invoice webHookUrl unless the call overrides it.
	CallbackURL string
	// Timeout bounds each SDK call; zero uses the SDK default of 5s.
	Timeout time.Duration
}

type Adapter struct {
	cfg    sdk.Config
	client *sdk.Client
}

var _ core.Gateway = (*Adapter)(nil)

func New(cfg Config) (*Adapter, error) {
	if err := core.RequireConfig(core.ProviderDigiPay,
		"BaseURL", cfg.BaseURL,
		"ClientID", cfg.ClientID,
		"ClientSecret", cfg.ClientSecret,
		"MerchantID", cfg.MerchantID,
		"MerchantName", cfg.MerchantName,
		"TerminalID", cfg.TerminalID,
	); err != nil {
		return nil, err
	}
	sdkCfg := sdk.Config{
		BaseURL:      cfg.BaseURL,
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		MerchantID:   cfg.MerchantID,
		MerchantName: cfg.MerchantName,
		TerminalID:   cfg.TerminalID,
		WebHookURL:   cfg.CallbackURL,
		Timeout:      cfg.Timeout,
	}
	return &Adapter{cfg: sdkCfg, client: sdk.NewClient(sdkCfg)}, nil
}

func (a *Adapter) Provider() core.Provider { return core.ProviderDigiPay }

func (a *Adapter) CreateInvoice(ctx context.Context, in core.CreateInvoiceInput) (*core.Invoice, error) {
	if err := a.InvoiceRules().Validate(a.Provider(), in); err != nil {
		return nil, err
	}
	bearer := strings.TrimSpace(in.Extra["bearer_token"])
	if bearer == "" {
		return nil, fmt.Errorf("%w: digipay requires Extra[\"bearer_token\"]", core.ErrInvalidInput)
	}
	if in.Amount <= 0 {
		return nil, fmt.Errorf("%w: amount must be positive", core.ErrInvalidInput)
	}

	// The SDK reads the webhook URL from its Config, so a per-call override
	// needs its own client.
	client := a.client
	if in.CallbackURL != "" && in.CallbackURL != a.cfg.WebHookURL {
		cfg := a.cfg
		cfg.WebHookURL = in.CallbackURL
		client = sdk.NewClient(cfg)
	}

	res, err := client.CreateUserInvoice(ctx, sdk.CreateUserInvoiceRequest{
		Amount:      in.Amount,
		BearerToken: bearer,
	})
	if err != nil {
		return nil, core.Wrap(core.ProviderDigiPay, "create_invoice", err)
	}
	if res.InvoiceID == "" {
		return nil, core.Wrap(core.ProviderDigiPay, "create_invoice", errors.New("response carried no invoice id"))
	}
	return &core.Invoice{
		Provider:   core.ProviderDigiPay,
		InvoiceID:  res.InvoiceID,
		OrderID:    in.OrderID,
		Amount:     in.Amount,
		PaymentURL: res.RedirectURL,
		Status:     core.StatusPending,
		Raw:        res,
	}, nil
}

func (a *Adapter) CheckInvoice(ctx context.Context, in core.CheckInvoiceInput) (*core.PaymentStatus, error) {
	if in.InvoiceID == "" {
		return nil, fmt.Errorf("%w: InvoiceID is required", core.ErrInvalidInput)
	}
	res, err := a.client.CheckInvoice(ctx, sdk.CheckInvoiceRequest{InvoiceID: in.InvoiceID})
	if err != nil {
		return nil, core.Wrap(core.ProviderDigiPay, "check_invoice", err)
	}
	ps := &core.PaymentStatus{
		Provider:      core.ProviderDigiPay,
		InvoiceID:     in.InvoiceID,
		Status:        mapStatus(res.Status),
		TransactionID: res.TxnID,
		Raw:           res,
	}
	if ps.Status == core.StatusPaid {
		ps.PaidAmount = res.Amount
	}
	return ps, nil
}

// mapStatus covers only the statuses the SDK itself treats as paid; DigiPay
// documents no failure or cancel values.
func mapStatus(s string) core.Status {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "success", "paid":
		return core.StatusPaid
	default:
		return core.StatusPending
	}
}
