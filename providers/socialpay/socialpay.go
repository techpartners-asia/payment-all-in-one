// Package socialpay adapts Golomt Bank's SocialPay in-store (POS) invoice API
// to core.Gateway, using github.com/techpartners-asia/golomt-api-go/socialpay.
//
// That package covers the same endpoints as github.com/techpartners-asia/socialpay-go
// but is the safer client: socialpay-go disables TLS certificate checks on
// http.DefaultTransport for the whole process on every call, prints to stdout,
// drops the response body from non-200 errors, and panics on settlement replies.
//
// CreateInvoice picks the invoice kind from the input:
//   - Phone set: SocialPay pushes the invoice to that phone's SocialPay app.
//     The Invoice carries no QR.
//   - Phone empty: SocialPay returns QR data, mapped to QRText.
//
// SocialPay identifies the invoice by the merchant's invoice string, which is
// the OrderID, so Invoice.InvoiceID equals OrderID. Every SocialPay request is
// signed over the amount, so CheckInvoice, CancelInvoice and Refund require
// Amount. The
// provider reads no Extra keys and has no per-invoice callback URL.
//
// CheckInvoice reports paid only on resp_code "00". SocialPay answers an
// unpaid invoice with an error header, which surfaces as *core.ProviderError.
//
// CancelInvoice voids an unpaid invoice. Refund cancels the whole payment;
// Amount must be the amount paid.
package socialpay

import (
	"context"
	"fmt"

	"github.com/techpartners-asia/golomt-api-go/socialpay"
	"github.com/techpartners-asia/payment-all-in-one/core"
)

const (
	invoiceAccepted = "SUCCESS"
	paymentApproved = "00"
)

type Config struct {
	Terminal string
	Secret   string
	// Endpoint is the SocialPay POS API base URL Golomt issued with the terminal.
	Endpoint string
}

type Adapter struct {
	client socialpay.SocialPay
}

var (
	_ core.Gateway  = (*Adapter)(nil)
	_ core.Refunder = (*Adapter)(nil)
	_ core.Canceler = (*Adapter)(nil)
)

func New(cfg Config) (*Adapter, error) {
	if err := core.RequireConfig(core.ProviderSocialPay,
		"Terminal", cfg.Terminal,
		"Secret", cfg.Secret,
		"Endpoint", cfg.Endpoint,
	); err != nil {
		return nil, err
	}
	return &Adapter{client: socialpay.New(cfg.Terminal, cfg.Secret, cfg.Endpoint)}, nil
}

func (a *Adapter) Provider() core.Provider { return core.ProviderSocialPay }

func (a *Adapter) CreateInvoice(_ context.Context, in core.CreateInvoiceInput) (*core.Invoice, error) {
	if in.OrderID == "" {
		return nil, fmt.Errorf("%w: socialpay requires OrderID", core.ErrInvalidInput)
	}
	if in.Amount <= 0 {
		return nil, fmt.Errorf("%w: socialpay requires a positive Amount", core.ErrInvalidInput)
	}

	var (
		res *socialpay.CommonResponse
		err error
	)
	if in.Phone != "" {
		res, err = a.client.CreateInvoicePhone(socialpay.InvoicePhoneInput{Phone: in.Phone, Amount: in.Amount, Invoice: in.OrderID})
	} else {
		res, err = a.client.CreateInvoiceQR(socialpay.InvoiceInput{Amount: in.Amount, Invoice: in.OrderID})
	}
	if err != nil {
		return nil, core.Wrap(core.ProviderSocialPay, "create_invoice", err)
	}
	// SocialPay can answer HTTP 200 / header 200 and still refuse the invoice;
	// desc then holds the reason instead of QR data.
	if res.Status != invoiceAccepted {
		return nil, core.Wrap(core.ProviderSocialPay, "create_invoice",
			fmt.Errorf("invoice not accepted: status %q: %s", res.Status, res.Description))
	}

	inv := &core.Invoice{
		Provider:  core.ProviderSocialPay,
		InvoiceID: in.OrderID,
		OrderID:   in.OrderID,
		Amount:    in.Amount,
		Status:    core.StatusPending,
		Raw:       res,
	}
	if in.Phone == "" {
		inv.QRText = res.Description
	}
	return inv, nil
}

func (a *Adapter) CheckInvoice(_ context.Context, in core.CheckInvoiceInput) (*core.PaymentStatus, error) {
	invoice := invoiceRef(in.InvoiceID, in.OrderID)
	if invoice == "" {
		return nil, fmt.Errorf("%w: socialpay check requires InvoiceID or OrderID", core.ErrInvalidInput)
	}
	if in.Amount <= 0 {
		return nil, fmt.Errorf("%w: socialpay check requires the invoice Amount", core.ErrInvalidInput)
	}
	res, err := a.client.CheckInvoice(socialpay.InvoiceInput{Amount: in.Amount, Invoice: invoice})
	if err != nil {
		return nil, core.Wrap(core.ProviderSocialPay, "check_invoice", err)
	}
	return &core.PaymentStatus{
		Provider:      core.ProviderSocialPay,
		InvoiceID:     invoice,
		Status:        paymentStatus(res.ResponseCode),
		PaidAmount:    res.Amount,
		TransactionID: res.ApprovalCode,
		Raw:           res,
	}, nil
}

func paymentStatus(respCode string) core.Status {
	if respCode == paymentApproved {
		return core.StatusPaid
	}
	return core.StatusPending
}

func (a *Adapter) Refund(_ context.Context, in core.RefundInput) error {
	invoice := invoiceRef(in.InvoiceID, in.OrderID)
	if invoice == "" {
		return fmt.Errorf("%w: socialpay refund requires InvoiceID or OrderID", core.ErrInvalidInput)
	}
	if in.Amount <= 0 {
		return fmt.Errorf("%w: socialpay refund requires Amount equal to the amount paid", core.ErrInvalidInput)
	}
	res, err := a.client.CancelPayment(socialpay.InvoiceInput{Amount: in.Amount, Invoice: invoice})
	if err != nil {
		return core.Wrap(core.ProviderSocialPay, "refund", err)
	}
	if res.ResponseCode != paymentApproved {
		return core.Wrap(core.ProviderSocialPay, "refund",
			fmt.Errorf("cancel not approved: resp_code %q: %s", res.ResponseCode, res.ResponseDescription))
	}
	return nil
}

func (a *Adapter) CancelInvoice(_ context.Context, in core.CancelInvoiceInput) error {
	invoice := invoiceRef(in.InvoiceID, in.OrderID)
	if invoice == "" {
		return fmt.Errorf("%w: socialpay cancel requires InvoiceID or OrderID", core.ErrInvalidInput)
	}
	if in.Amount <= 0 {
		return fmt.Errorf("%w: socialpay cancel requires the invoice Amount", core.ErrInvalidInput)
	}
	res, err := a.client.CancelInvoice(socialpay.InvoiceInput{Amount: in.Amount, Invoice: invoice})
	if err != nil {
		return core.Wrap(core.ProviderSocialPay, "cancel_invoice", err)
	}
	if res.Status != invoiceAccepted {
		return core.Wrap(core.ProviderSocialPay, "cancel_invoice",
			fmt.Errorf("cancel not accepted: status %q: %s", res.Status, res.Description))
	}
	return nil
}

func invoiceRef(invoiceID, orderID string) string {
	if invoiceID != "" {
		return invoiceID
	}
	return orderID
}
