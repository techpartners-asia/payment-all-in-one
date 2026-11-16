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
	// mu serializes SDK calls: the SDK caches its access token in unguarded fields.
	mu  sync.Mutex
	sdk storepaysdk.Storepay
}

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
	return &Adapter{cfg: cfg, sdk: newSDK(cfg, cfg.CallbackURL)}, nil
}

func newSDK(cfg Config, callbackURL string) storepaysdk.Storepay {
	// The SDK splices the app credentials into the token URL's query string
	// unescaped, and uses them nowhere else.
	return storepaysdk.New(
		url.QueryEscape(cfg.AppUsername), url.QueryEscape(cfg.AppPassword),
		cfg.Username, cfg.Password, cfg.AuthURL, cfg.BaseURL, cfg.StoreID, callbackURL,
	)
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

	// The SDK fixes the callback URL at construction, so a per-call override
	// needs its own client (and its own token request).
	sdk := a.sdk
	if in.CallbackURL != "" && in.CallbackURL != a.cfg.CallbackURL {
		sdk = newSDK(a.cfg, in.CallbackURL)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

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
		return a.sdk.LoanCheck(url.PathEscape(in.InvoiceID))
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
