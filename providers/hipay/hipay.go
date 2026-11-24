// Package hipay adapts the HiPay payment API (github.com/techpartners-asia/hipay-go/hipay)
// to core.Gateway.
//
// CreateInvoice opens a HiPay checkout with QR data; InvoiceID is HiPay's checkoutId.
// The SDK sends only the amount, so HiPay never sees the order id, description, callback
// or return URL, and this adapter reads no Extra keys. Poll CheckInvoice for the outcome.
//
// Optional operations:
//   - core.Canceler voids an unpaid checkout.
//   - core.Refunder reverses a whole payment through /payment/cancel. It needs
//     RefundInput.TransactionID, the paymentId CheckInvoice returns as TransactionID.
//     HiPay accepts reversals only on the day of payment and has no partial refund.
//
// The SDK takes no context, so ctx is not propagated.
package hipay

import (
	"context"
	"fmt"
	"strings"

	"github.com/techpartners-asia/hipay-go/hipay"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

type Config struct {
	// Endpoint is the API base URL, e.g. https://api.hipay.mn.
	Endpoint string
	// Token is the merchant client_secret, sent as the bearer token.
	Token string
	// EntityID is the merchant client_id.
	EntityID string
}

type Adapter struct {
	client hipay.Hipay
}

var (
	_ core.Gateway  = (*Adapter)(nil)
	_ core.Canceler = (*Adapter)(nil)
	_ core.Refunder = (*Adapter)(nil)
)

func New(cfg Config) (*Adapter, error) {
	if err := core.RequireConfig(core.ProviderHiPay,
		"Endpoint", cfg.Endpoint,
		"Token", cfg.Token,
		"EntityID", cfg.EntityID,
	); err != nil {
		return nil, err
	}
	return &Adapter{client: hipay.New(strings.TrimSuffix(cfg.Endpoint, "/"), cfg.Token, cfg.EntityID)}, nil
}

func (a *Adapter) Provider() core.Provider { return core.ProviderHiPay }

func (a *Adapter) CreateInvoice(_ context.Context, in core.CreateInvoiceInput) (*core.Invoice, error) {
	if err := a.InvoiceRules().Validate(a.Provider(), in); err != nil {
		return nil, err
	}
	res, err := a.client.Checkout(in.Amount)
	if err != nil {
		return nil, core.Wrap(core.ProviderHiPay, "create_invoice", err)
	}
	return &core.Invoice{
		Provider:  core.ProviderHiPay,
		InvoiceID: res.CheckoutID,
		OrderID:   in.OrderID,
		Amount:    in.Amount,
		QRText:    res.QrData,
		Status:    core.StatusPending,
		Raw:       res,
	}, nil
}

func (a *Adapter) CheckInvoice(_ context.Context, in core.CheckInvoiceInput) (*core.PaymentStatus, error) {
	if in.InvoiceID == "" {
		return nil, fmt.Errorf("%w: hipay check requires InvoiceID (checkoutId)", core.ErrInvalidInput)
	}
	res, err := a.client.CheckoutGet(in.InvoiceID)
	if err != nil {
		return nil, core.Wrap(core.ProviderHiPay, "check_invoice", err)
	}
	st := &core.PaymentStatus{
		Provider:      core.ProviderHiPay,
		InvoiceID:     in.InvoiceID,
		Status:        mapStatus(res.Status),
		TransactionID: res.PaymentID,
		Raw:           res,
	}
	if st.Status == core.StatusPaid {
		st.PaidAmount = res.Amount
	}
	return st, nil
}

func (a *Adapter) CancelInvoice(_ context.Context, in core.CancelInvoiceInput) error {
	if in.InvoiceID == "" {
		return fmt.Errorf("%w: hipay cancel requires InvoiceID (checkoutId)", core.ErrInvalidInput)
	}
	_, err := a.client.CheckoutCancel(in.InvoiceID)
	return core.Wrap(core.ProviderHiPay, "cancel_invoice", err)
}

func (a *Adapter) Refund(_ context.Context, in core.RefundInput) error {
	if in.TransactionID == "" {
		return fmt.Errorf("%w: hipay refund requires TransactionID (paymentId)", core.ErrInvalidInput)
	}
	if in.Amount != 0 {
		return core.Wrap(core.ProviderHiPay, "refund",
			fmt.Errorf("%w: hipay refunds only the whole payment; pass Amount 0", core.ErrUnsupported))
	}
	_, err := a.client.PaymentCancel(in.TransactionID)
	return core.Wrap(core.ProviderHiPay, "refund", err)
}

// statuses holds HiPay's documented checkout statuses (developers.hipay.mn/checkout-get).
var statuses = map[string]core.Status{
	"new":      core.StatusPending,
	"paid":     core.StatusPaid,
	"canceled": core.StatusCancelled,
	"expired":  core.StatusFailed,
	"invalid":  core.StatusFailed,
}

func mapStatus(s string) core.Status {
	if st, ok := statuses[strings.ToLower(s)]; ok {
		return st
	}
	return core.StatusPending
}
