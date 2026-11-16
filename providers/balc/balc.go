// Package balc adapts Balanced Growth Credit (BALC)
// (github.com/techpartners-asia/balc-api-go) to core.Gateway.
//
// BALC has no invoices. CreateInvoice draws a loan advance on the customer's
// BALC credit line (in.CustomerID, the numeric BALC customer id). The advance is
// granted synchronously, so a successful CreateInvoice returns StatusPaid and
// InvoiceID is the BALC account id the advance was booked to. Before drawing,
// CreateInvoice checks the customer's limit and fails with ErrCreditUnavailable
// when the line is disabled or short of in.Amount. The loan description is
// in.Description, or in.OrderID when that is empty.
//
// BALC exposes no status lookup, so CheckInvoice returns core.ErrUnsupported.
// No Extra keys are read. Canceler and Refunder are not implemented: the SDK
// has no cancel or refund call.
package balc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	balcapi "github.com/techpartners-asia/balc-api-go"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

// ErrCreditUnavailable means the customer's credit line is disabled or its
// available limit is below the requested amount.
var ErrCreditUnavailable = errors.New("balc: credit unavailable")

// limitUsable is LimitResponse.Status for a line the customer may draw on.
const limitUsable = 1

type Config struct {
	Endpoint string
	Token    string
}

type Adapter struct {
	sdk balcapi.Balc
}

var _ core.Gateway = (*Adapter)(nil)

func New(cfg Config) (*Adapter, error) {
	if err := core.RequireConfig(core.ProviderBalc,
		"Endpoint", cfg.Endpoint,
		"Token", cfg.Token,
	); err != nil {
		return nil, err
	}
	return &Adapter{sdk: balcapi.New(cfg.Endpoint, cfg.Token)}, nil
}

func (a *Adapter) Provider() core.Provider { return core.ProviderBalc }

func (a *Adapter) CreateInvoice(_ context.Context, in core.CreateInvoiceInput) (*core.Invoice, error) {
	if err := a.InvoiceRules().Validate(a.Provider(), in); err != nil {
		return nil, err
	}
	customerID, err := strconv.Atoi(in.CustomerID)
	if err != nil {
		return nil, fmt.Errorf("%w: customer id must be a BALC numeric customer id, got %q", core.ErrInvalidInput, in.CustomerID)
	}
	if in.Amount <= 0 || in.Amount != math.Trunc(in.Amount) {
		return nil, fmt.Errorf("%w: amount must be a positive whole number, got %v", core.ErrInvalidInput, in.Amount)
	}
	description := in.Description
	if description == "" {
		description = in.OrderID
	}

	limit, err := recoverSDK(func() (balcapi.LimitResponse, error) {
		return a.sdk.LimitCheck(customerID)
	})
	if err != nil {
		return nil, core.Wrap(core.ProviderBalc, "create_invoice", fmt.Errorf("limit check: %w", err))
	}
	if limit.Status != limitUsable || limit.AvailLimit < in.Amount {
		return nil, core.Wrap(core.ProviderBalc, "create_invoice",
			fmt.Errorf("%w: status %d, available %v, requested %v", ErrCreditUnavailable, limit.Status, limit.AvailLimit, in.Amount))
	}

	accountID, err := recoverSDK(func() (string, error) {
		return a.sdk.Loan(int(in.Amount), description, customerID)
	})
	if err != nil {
		return nil, core.Wrap(core.ProviderBalc, "create_invoice", err)
	}
	// The SDK ignores the body of a 200 that is not a JSON string, so an empty
	// id means the advance cannot be confirmed.
	if accountID == "" {
		return nil, core.Wrap(core.ProviderBalc, "create_invoice", errors.New("loan response carried no account id"))
	}

	return &core.Invoice{
		Provider:  core.ProviderBalc,
		InvoiceID: accountID,
		OrderID:   in.OrderID,
		Amount:    in.Amount,
		Status:    core.StatusPaid,
		Raw:       accountID,
	}, nil
}

func (a *Adapter) CheckInvoice(context.Context, core.CheckInvoiceInput) (*core.PaymentStatus, error) {
	return nil, core.Wrap(core.ProviderBalc, "check_invoice", core.ErrUnsupported)
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
