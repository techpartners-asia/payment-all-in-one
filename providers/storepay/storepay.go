// Package storepay adapts the StorePay buy-now-pay-later gateway
// (github.com/techpartners-asia/storepay-go) to core.Gateway.
//
// CreateInvoice requests a loan for in.Amount against the customer's mobile
// number (in.Phone, required); StorePay then asks the customer to confirm it in
// their app. InvoiceID is the StorePay loan id that CheckInvoice polls. The loan
// description is in.Description, or in.OrderID when that is empty, so the loan
// can be matched to the order on StorePay's side.
//
// StorePay reports only whether a loan is confirmed, not an amount, so
// PaymentStatus.PaidAmount is always zero.
//
// No Extra keys are read. Canceler and Refunder are not implemented: the SDK
// has no cancel or refund call.
package storepay

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"sync"

	storepaysdk "github.com/techpartners-asia/storepay-go"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

type Config struct {
	// AppUsername and AppPassword are the OAuth password-grant credentials.
	AppUsername string
	AppPassword string
	// Username and Password are the OAuth client (basic auth) credentials.
	Username    string
	Password    string
	AuthURL     string
	BaseURL     string
	StoreID     string
	CallbackURL string
}

type Adapter struct {
	cfg Config
	// mu serializes SDK calls and guards clients: the SDK caches its access
	// token in unguarded fields.
	mu sync.Mutex
	// clients holds one SDK client per callback URL. The SDK fixes the
	// callback at construction and caches its token per client, so reusing a
	// client is what keeps repeated callback overrides from logging in again.
	clients map[string]storepaysdk.Storepay
}

// maxClients bounds clients so per-order callback URLs cannot grow it without
// limit. Past it, an unseen callback URL gets a client used once.
const maxClients = 16

var _ core.Gateway = (*Adapter)(nil)

func New(cfg Config) (*Adapter, error) {
	if err := core.RequireConfig(core.ProviderStorePay,
		"AppUsername", cfg.AppUsername,
		"AppPassword", cfg.AppPassword,
		"Username", cfg.Username,
		"Password", cfg.Password,
		"AuthURL", cfg.AuthURL,
		"BaseURL", cfg.BaseURL,
		"StoreID", cfg.StoreID,
	); err != nil {
		return nil, err
	}
	return &Adapter{cfg: cfg, clients: map[string]storepaysdk.Storepay{}}, nil
}

// client returns the SDK client for callbackURL. The caller holds mu.
func (a *Adapter) client(callbackURL string) storepaysdk.Storepay {
	if c, ok := a.clients[callbackURL]; ok {
		return c
	}
	// The SDK splices the app credentials into the token URL's query string
	// unescaped, and uses them nowhere else.
	c := storepaysdk.New(
		url.QueryEscape(a.cfg.AppUsername), url.QueryEscape(a.cfg.AppPassword),
		a.cfg.Username, a.cfg.Password, a.cfg.AuthURL, a.cfg.BaseURL, a.cfg.StoreID, callbackURL,
	)
	if len(a.clients) < maxClients {
		a.clients[callbackURL] = c
	}
	return c
}

func (a *Adapter) Provider() core.Provider { return core.ProviderStorePay }

func (a *Adapter) CreateInvoice(_ context.Context, in core.CreateInvoiceInput) (*core.Invoice, error) {
	if err := a.InvoiceRules().Validate(a.Provider(), in); err != nil {
		return nil, err
	}
	if in.Phone == "" {
		return nil, fmt.Errorf("%w: phone is required", core.ErrInvalidInput)
	}
	if in.Amount <= 0 {
		return nil, fmt.Errorf("%w: amount must be positive, got %v", core.ErrInvalidInput, in.Amount)
	}
	description := in.Description
	if description == "" {
		description = in.OrderID
	}

	callbackURL := a.cfg.CallbackURL
	if in.CallbackURL != "" {
		callbackURL = in.CallbackURL
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	sdk := a.client(callbackURL)

	loanID, err := recoverSDK(func() (int64, error) {
		return sdk.Loan(storepaysdk.StorepayLoanInput{
			Description:  description,
			MobileNumber: in.Phone,
			Amount:       in.Amount,
		})
	})
	if err != nil {
		return nil, core.Wrap(core.ProviderStorePay, "create_invoice", err)
	}

	return &core.Invoice{
		Provider:  core.ProviderStorePay,
		InvoiceID: strconv.FormatInt(loanID, 10),
		OrderID:   in.OrderID,
		Amount:    in.Amount,
		Status:    core.StatusPending,
		Raw:       loanID,
	}, nil
}

func (a *Adapter) CheckInvoice(_ context.Context, in core.CheckInvoiceInput) (*core.PaymentStatus, error) {
	if in.InvoiceID == "" {
		return nil, fmt.Errorf("%w: invoice id is required", core.ErrInvalidInput)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	confirmed, err := recoverSDK(func() (bool, error) {
		return a.client(a.cfg.CallbackURL).LoanCheck(url.PathEscape(in.InvoiceID))
	})
	if err != nil {
		return nil, core.Wrap(core.ProviderStorePay, "check_invoice", err)
	}

	return &core.PaymentStatus{
		Provider:  core.ProviderStorePay,
		InvoiceID: in.InvoiceID,
		Status:    mapStatus(confirmed),
		Raw:       confirmed,
	}, nil
}

// mapStatus maps LoanCheck's value. StorePay does not distinguish a declined
// loan from one awaiting confirmation, so false stays pending.
func mapStatus(confirmed bool) core.Status {
	if confirmed {
		return core.StatusPaid
	}
	return core.StatusPending
}

// recoverSDK turns SDK panics into errors: the SDK dereferences a nil
// *http.Response when the transport fails, and indexes an empty msgList on
// some error responses.
func recoverSDK[T any](fn func() (T, error)) (out T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("sdk panic: %v", r)
		}
	}()
	return fn()
}
