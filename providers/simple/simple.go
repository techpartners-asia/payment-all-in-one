// Package simple adapts the Simple buy-now-pay-later gateway
// (github.com/techpartners-asia/simple-go) to core.Gateway.
//
// CreateInvoice creates a Simple invoice keyed by the merchant OrderID. When
// in.Phone is set, the invoice is also assigned to that mobile number so it
// appears in the customer's Simple app. InvoiceID equals OrderID because
// Simple checks invoices by order id. The provider's invoice UUID is in Raw.
//
// No Extra keys are read. Canceler and Refunder are not implemented: the SDK
// has no cancel or refund call.
package simple

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sync"
	"time"

	simplesdk "github.com/techpartners-asia/simple-go"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

const defaultExpiresIn = 20 * time.Minute

// Simple reads expire_date as wall-clock time in Mongolia (UTC+8, no DST).
var mongoliaTime = time.FixedZone("ULAT", 8*60*60)

type Config struct {
	Username    string
	Password    string
	BaseURL     string
	CallbackURL string
}

type Adapter struct {
	cfg Config
	// mu serializes SDK calls: the SDK caches its access token in unguarded fields.
	mu  sync.Mutex
	sdk simplesdk.Simple
}

var _ core.Gateway = (*Adapter)(nil)

func New(cfg Config) (*Adapter, error) {
	if err := core.RequireConfig(core.ProviderSimple,
		"Username", cfg.Username,
		"Password", cfg.Password,
		"BaseURL", cfg.BaseURL,
	); err != nil {
		return nil, err
	}
	return &Adapter{
		cfg: cfg,
		sdk: simplesdk.New(cfg.Username, cfg.Password, cfg.BaseURL, cfg.CallbackURL),
	}, nil
}

func (a *Adapter) Provider() core.Provider { return core.ProviderSimple }

func (a *Adapter) CreateInvoice(_ context.Context, in core.CreateInvoiceInput) (*core.Invoice, error) {
	if err := a.InvoiceRules().Validate(a.Provider(), in); err != nil {
		return nil, err
	}
	if in.OrderID == "" {
		return nil, fmt.Errorf("%w: order id is required", core.ErrInvalidInput)
	}
	if in.Amount <= 0 || in.Amount != math.Trunc(in.Amount) {
		return nil, fmt.Errorf("%w: amount must be a positive whole number, got %v", core.ErrInvalidInput, in.Amount)
	}
	expiresIn := in.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = defaultExpiresIn
	}
	expiresAt := time.Now().Add(expiresIn)

	// The SDK fixes the callback URL at construction, so a per-call override
	// needs its own client (and its own token request).
	sdk := a.sdk
	if in.CallbackURL != "" && in.CallbackURL != a.cfg.CallbackURL {
		sdk = simplesdk.New(a.cfg.Username, a.cfg.Password, a.cfg.BaseURL, in.CallbackURL)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	res, err := recoverSDK(func() (simplesdk.SimpleCreateInvoiceResponse, error) {
		return sdk.CreateInvoice(simplesdk.SimpleCreateInvoiceInput{
			OrderID:    in.OrderID,
			Total:      int(in.Amount),
			ExpireDate: expiresAt.In(mongoliaTime).Format(time.DateTime),
		})
	})
	if err != nil {
		return nil, core.Wrap(core.ProviderSimple, "create_invoice", err)
	}
	// The SDK returns nil error for any HTTP 200, including error codes; only
	// an invoice uuid proves the invoice exists.
	if res.Data == "" {
		return nil, core.Wrap(core.ProviderSimple, "create_invoice",
			fmt.Errorf("no invoice uuid in response: code %q: %s", res.Code, res.Message))
	}

	if in.Phone != "" {
		_, err := recoverSDK(func() (simplesdk.SimpleSendInvoiceToNumberResponse, error) {
			return sdk.SendInvoiceToNumber(simplesdk.SimpleSendInvoiceToNumberRequest{
				InvoiceUUID: res.Data,
				Mobile:      in.Phone,
			})
		})
		if err != nil {
			return nil, core.Wrap(core.ProviderSimple, "create_invoice", fmt.Errorf("assign invoice %s to mobile: %w", res.Data, err))
		}
	}

	return &core.Invoice{
		Provider:  core.ProviderSimple,
		InvoiceID: in.OrderID,
		OrderID:   in.OrderID,
		Amount:    in.Amount,
		Status:    core.StatusPending,
		ExpiresAt: expiresAt,
		Raw:       res,
	}, nil
}

func (a *Adapter) CheckInvoice(_ context.Context, in core.CheckInvoiceInput) (*core.PaymentStatus, error) {
	if in.InvoiceID == "" {
		return nil, fmt.Errorf("%w: invoice id is required", core.ErrInvalidInput)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// The SDK appends order_id to the query string without escaping it.
	res, err := recoverSDK(func() (simplesdk.SimpleSendInvoiceToNumberResponse, error) {
		return a.sdk.GetInvoice(simplesdk.SimpleGetInvoiceRequest{OrderID: url.QueryEscape(in.InvoiceID)})
	})
	if err != nil {
		return nil, core.Wrap(core.ProviderSimple, "check_invoice", err)
	}

	status := mapStatus(res.Data.InvoiceStatus)
	out := &core.PaymentStatus{
		Provider:  core.ProviderSimple,
		InvoiceID: in.InvoiceID,
		Status:    status,
		Raw:       res,
	}
	if status == core.StatusPaid {
		out.PaidAmount = res.Data.Total
	}
	return out, nil
}

// statuses holds the invoice_status values the SDK documents.
var statuses = map[string]core.Status{
	"PAID":      core.StatusPaid,
	"COMPLETED": core.StatusPaid,
}

func mapStatus(s string) core.Status {
	if st, ok := statuses[s]; ok {
		return st
	}
	return core.StatusPending
}

// recoverSDK turns SDK panics into errors: the SDK dereferences a nil
// *http.Response when the transport fails.
func recoverSDK[T any](fn func() (T, error)) (out T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("sdk panic: %v", r)
		}
	}()
	return fn()
}
