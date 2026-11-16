// Package tino adapts the Tino payment gateway (github.com/techpartners-asia/tino-go)
// to core.Gateway.
//
// The SDK leaves the merchant token to its caller, so the adapter logs in on first use,
// keeps the token until shortly before it expires, and logs in again once when Tino
// rejects it with 401/403.
//
// Extra keys read by CreateInvoice:
//   - "wallet_id": receiving wallet, overriding Config.WalletID.
//   - "meta_data": free-form string stored on the invoice.
//
// Optional operations: core.Canceler. Tino has no refund endpoint.
package tino

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	tino "github.com/techpartners-asia/tino-go"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

// tokenRefreshMargin renews the token early so a request never leaves with a token that
// expires in flight.
const tokenRefreshMargin = 30 * time.Second

type Config struct {
	// AuthURL is the merchant login host, e.g. https://auth.tino.mn/api/v1.
	AuthURL string
	// BaseURL is the payment API host, e.g. https://payment.tino.mn/api/v1.
	BaseURL  string
	Username string
	Password string
	// WalletID is the merchant wallet that receives payments.
	WalletID    string
	CallbackURL string
}

type Adapter struct {
	cfg    Config
	client tino.Tino
	// mu serializes logins so concurrent callers with an expired token share one Login.
	mu sync.Mutex
}

var (
	_ core.Gateway  = (*Adapter)(nil)
	_ core.Canceler = (*Adapter)(nil)
)

func New(cfg Config) (*Adapter, error) {
	if err := core.RequireConfig(core.ProviderTino,
		"AuthURL", cfg.AuthURL,
		"BaseURL", cfg.BaseURL,
		"Username", cfg.Username,
		"Password", cfg.Password,
		"WalletID", cfg.WalletID,
	); err != nil {
		return nil, err
	}
	return &Adapter{
		cfg:    cfg,
		client: tino.New(cfg.AuthURL, cfg.BaseURL, cfg.Username, cfg.Password),
	}, nil
}

func (a *Adapter) Provider() core.Provider { return core.ProviderTino }

func (a *Adapter) CreateInvoice(ctx context.Context, in core.CreateInvoiceInput) (*core.Invoice, error) {
	if err := a.InvoiceRules().Validate(a.Provider(), in); err != nil {
		return nil, err
	}
	req := &tino.InvoiceRequest{
		WalletID:      a.cfg.WalletID,
		ExpiresMinute: int(math.Ceil(in.ExpiresIn.Minutes())),
		Description:   in.Description,
		Amount:        in.Amount,
		TransactionID: in.OrderID,
		CallbackURL:   a.cfg.CallbackURL,
		MetaData:      in.Extra["meta_data"],
	}
	if w := in.Extra["wallet_id"]; w != "" {
		req.WalletID = w
	}
	if in.CallbackURL != "" {
		req.CallbackURL = in.CallbackURL
	}

	var res *tino.InvoiceResponse
	err := a.authorized(ctx, func() (err error) {
		res, err = a.client.CreateInvoice(req)
		return err
	})
	if err != nil {
		return nil, core.Wrap(core.ProviderTino, "create_invoice", err)
	}

	inv := &core.Invoice{
		Provider:  core.ProviderTino,
		InvoiceID: res.Data.InvoiceID,
		OrderID:   in.OrderID,
		Amount:    res.Data.Amount,
		QRText:    res.Data.QrCode,
		Status:    mapStatus(res.Data.Status),
		ExpiresAt: res.Data.ExpiresAt,
		Raw:       res,
	}
	if res.Data.DeepLink != "" {
		inv.Deeplinks = []core.Deeplink{{Name: "Tino", Link: res.Data.DeepLink}}
	}
	return inv, nil
}

func (a *Adapter) CheckInvoice(ctx context.Context, in core.CheckInvoiceInput) (*core.PaymentStatus, error) {
	if in.InvoiceID == "" {
		return nil, fmt.Errorf("%w: tino check requires InvoiceID", core.ErrInvalidInput)
	}

	var res *tino.InvoiceCheckResponse
	err := a.authorized(ctx, func() (err error) {
		res, err = a.client.CheckInvoice(in.InvoiceID)
		return err
	})
	if err != nil {
		return nil, core.Wrap(core.ProviderTino, "check_invoice", err)
	}

	st := &core.PaymentStatus{
		Provider:  core.ProviderTino,
		InvoiceID: in.InvoiceID,
		Status:    mapStatus(res.Data.Status),
		Raw:       res,
	}
	if st.Status == core.StatusPaid {
		st.PaidAmount = res.Data.Amount
	}
	return st, nil
}

func (a *Adapter) CancelInvoice(ctx context.Context, in core.CancelInvoiceInput) error {
	if in.InvoiceID == "" {
		return fmt.Errorf("%w: tino cancel requires InvoiceID", core.ErrInvalidInput)
	}
	err := a.authorized(ctx, func() error {
		_, err := a.client.CancelInvoice(in.InvoiceID)
		return err
	})
	return core.Wrap(core.ProviderTino, "cancel_invoice", err)
}

// authorized runs call with a valid token, logging in again and retrying once if Tino
// rejects the token. The SDK guarantees a rejected request was never processed, so the
// retry cannot double-create an invoice.
func (a *Adapter) authorized(ctx context.Context, call func() error) error {
	used, err := a.token(ctx, "")
	if err != nil {
		return err
	}
	err = call()
	if !errors.Is(err, tino.ErrUnauthorized) {
		return err
	}
	if _, err := a.token(ctx, used); err != nil {
		return err
	}
	return call()
}

// token returns the installed access token, logging in when there is none, when it is
// about to expire, or when it is the one Tino just rejected. Comparing against rejected
// keeps concurrent 401s from triggering one login each.
func (a *Adapter) token(ctx context.Context, rejected string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	cur := a.client.Token()
	if !cur.IsZero() && cur.AccessToken != rejected && time.Until(cur.ExpiresAt) > tokenRefreshMargin {
		return cur.AccessToken, nil
	}
	fresh, err := a.client.Login(ctx)
	if err != nil {
		return "", err
	}
	a.client.SetToken(fresh)
	return fresh.AccessToken, nil
}

var statuses = map[string]core.Status{
	"created":   core.StatusPending,
	"pending":   core.StatusPending,
	"paid":      core.StatusPaid,
	"cancelled": core.StatusCancelled,
	"canceled":  core.StatusCancelled,
	"expired":   core.StatusFailed,
}

func mapStatus(s string) core.Status {
	if st, ok := statuses[strings.ToLower(s)]; ok {
		return st
	}
	return core.StatusPending
}
