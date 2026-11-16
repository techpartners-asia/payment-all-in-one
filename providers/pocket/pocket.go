// Package pocket adapts Pocket (InvesCore) merchant invoicing
// (github.com/techpartners-asia/pocket-go) to core.Gateway.
//
// pocket-go leaves the access token to its caller, so the adapter logs in on
// first use, keeps the token until shortly before it expires, and on a
// rejected token logs in again and retries the call once. Pocket documents
// that a rejected request was never processed, so the retry cannot create a
// second invoice.
//
// Invoice.InvoiceID is Pocket's invoice id. CheckInvoice looks the invoice up
// by InvoiceID, or by OrderID (Pocket's orderNumber) when InvoiceID is empty.
//
// Extra keys read by CreateInvoice:
//   - invoice_type: ZERO (default, Pocket Zero instalments) or PURCHASE (wallet).
//   - channel: Pocket sales channel, default "ecommerce".
//
// Pocket's SDK has no cancel or refund call, so neither Canceler nor Refunder
// is implemented.
package pocket

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/techpartners-asia/payment-all-in-one/core"
	pocketsdk "github.com/techpartners-asia/pocket-go"
)

// tokenSkew renews the token this long before Pocket's stated expiry so a
// request in flight does not carry a token that lapses mid-call.
const tokenSkew = 30 * time.Second

type Config struct {
	ClientID     string
	ClientSecret string
	TerminalID   int64
	// Environment is "production" (default) or "sandbox".
	Environment string
}

type Adapter struct {
	client pocketsdk.Pocket
	now    func() time.Time
	// mu serialises logins so concurrent calls on an expired token share one.
	mu sync.Mutex
}

var _ core.Gateway = (*Adapter)(nil)

func New(cfg Config) (*Adapter, error) {
	if err := core.RequireConfig(core.ProviderPocket,
		"ClientID", cfg.ClientID,
		"ClientSecret", cfg.ClientSecret,
	); err != nil {
		return nil, err
	}
	if cfg.TerminalID == 0 {
		return nil, fmt.Errorf("%w: %s missing [terminal_id]", core.ErrInvalidConfig, core.ProviderPocket)
	}
	env := cfg.Environment
	switch env {
	case "":
		env = "production"
	case "production", "sandbox":
	default:
		// pocket-go silently falls back to production on an unknown value,
		// which would send sandbox credentials to the live API.
		return nil, fmt.Errorf("%w: %s environment %q, want production or sandbox", core.ErrInvalidConfig, core.ProviderPocket, cfg.Environment)
	}
	// pocket-go stores its merchant argument but never sends it.
	return newAdapter(pocketsdk.New("", cfg.ClientID, cfg.ClientSecret, env, cfg.TerminalID), time.Now), nil
}

func newAdapter(client pocketsdk.Pocket, now func() time.Time) *Adapter {
	return &Adapter{client: client, now: now}
}

func (a *Adapter) Provider() core.Provider { return core.ProviderPocket }

func (a *Adapter) CreateInvoice(ctx context.Context, in core.CreateInvoiceInput) (*core.Invoice, error) {
	if in.OrderID == "" {
		return nil, fmt.Errorf("%w: pocket requires OrderID", core.ErrInvalidInput)
	}
	if in.Amount <= 0 {
		return nil, fmt.Errorf("%w: pocket requires a positive Amount", core.ErrInvalidInput)
	}
	invoiceType := strings.ToUpper(in.Extra["invoice_type"])
	switch invoiceType {
	case "":
		invoiceType = "ZERO"
	case "ZERO", "PURCHASE":
	default:
		return nil, fmt.Errorf("%w: pocket invoice_type %q, want ZERO or PURCHASE", core.ErrInvalidInput, in.Extra["invoice_type"])
	}
	channel := in.Extra["channel"]
	if channel == "" {
		channel = "ecommerce"
	}
	info := in.Description
	if info == "" {
		info = in.OrderID
	}

	res, err := withToken(ctx, a, func() (pocketsdk.PocketCreateInvoiceResponse, error) {
		return a.client.CreateInvoice(pocketsdk.PocketCreateInvoiceInput{
			Amount:      in.Amount,
			Info:        info,
			OrderNumber: in.OrderID,
			InvoiceType: invoiceType,
			Channel:     channel,
		})
	})
	if err != nil {
		return nil, core.Wrap(core.ProviderPocket, "create_invoice", err)
	}
	// pocket-go ignores JSON decode errors, so an unreadable reply arrives as
	// a zero response rather than an error.
	if res.ID == 0 {
		return nil, core.Wrap(core.ProviderPocket, "create_invoice", errors.New("response carried no invoice id"))
	}

	inv := &core.Invoice{
		Provider:  core.ProviderPocket,
		InvoiceID: strconv.FormatUint(uint64(res.ID), 10),
		OrderID:   in.OrderID,
		Amount:    in.Amount,
		QRText:    res.Qr,
		Status:    core.StatusPending,
		Raw:       res,
	}
	if res.DeepLink != "" {
		inv.Deeplinks = []core.Deeplink{{Name: "Pocket", Link: res.DeepLink}}
	}
	return inv, nil
}

func (a *Adapter) CheckInvoice(ctx context.Context, in core.CheckInvoiceInput) (*core.PaymentStatus, error) {
	var lookup func() (pocketsdk.PocketInvoiceDetailResponse, error)
	switch {
	case in.InvoiceID != "":
		lookup = func() (pocketsdk.PocketInvoiceDetailResponse, error) {
			return a.client.GetInvoiceByInvoiceID(in.InvoiceID)
		}
	case in.OrderID != "":
		lookup = func() (pocketsdk.PocketInvoiceDetailResponse, error) {
			return a.client.GetInvoiceByOrderNumber(in.OrderID)
		}
	default:
		return nil, fmt.Errorf("%w: pocket check requires InvoiceID or OrderID", core.ErrInvalidInput)
	}

	res, err := withToken(ctx, a, lookup)
	if err != nil {
		return nil, core.Wrap(core.ProviderPocket, "check_invoice", err)
	}
	if res.State == "" {
		return nil, core.Wrap(core.ProviderPocket, "check_invoice", errors.New("response carried no invoice state"))
	}

	st := &core.PaymentStatus{
		Provider:  core.ProviderPocket,
		InvoiceID: in.InvoiceID,
		Status:    invoiceStatus(res.State),
		Raw:       res,
	}
	if res.ID != 0 {
		st.InvoiceID = strconv.FormatUint(uint64(res.ID), 10)
	}
	if st.Status == core.StatusPaid {
		st.PaidAmount = res.Amount
	}
	return st, nil
}

// invoiceStatus maps Pocket's invoice states (pending, processing, processed,
// paid, cancelled, rejected, unsuccess). Only "paid" is a settled payment.
func invoiceStatus(state string) core.Status {
	switch state {
	case "paid":
		return core.StatusPaid
	case "cancelled":
		return core.StatusCancelled
	case "rejected", "unsuccess":
		return core.StatusFailed
	default:
		return core.StatusPending
	}
}

// withToken runs call with a usable token installed, logging in again and
// retrying once when Pocket rejects the token.
func withToken[T any](ctx context.Context, a *Adapter, call func() (T, error)) (T, error) {
	var zero T
	used, err := a.ensureToken(ctx, "")
	if err != nil {
		return zero, err
	}
	res, err := call()
	if !errors.Is(err, pocketsdk.ErrUnauthorized) {
		return res, err
	}
	if _, err := a.ensureToken(ctx, used); err != nil {
		return zero, err
	}
	return call()
}

// ensureToken returns the installed access token, first logging in when it is
// missing, near expiry, or equal to rejected. A token another call already
// replaced is reused rather than triggering a second login.
func (a *Adapter) ensureToken(ctx context.Context, rejected string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur := a.client.Token()
	if cur.AccessToken != rejected && usable(cur, a.now()) {
		return cur.AccessToken, nil
	}
	tok, err := a.client.Login(ctx)
	if err != nil {
		return "", err
	}
	a.client.SetToken(tok)
	return tok.AccessToken, nil
}

// usable treats a token without an expiry as spent, as pocket-go's Token
// documents: Pocket gave no lifetime, so it is good for one call only.
func usable(t pocketsdk.Token, now time.Time) bool {
	return !t.IsZero() && !t.ExpiresAt.IsZero() && now.Add(tokenSkew).Before(t.ExpiresAt)
}
