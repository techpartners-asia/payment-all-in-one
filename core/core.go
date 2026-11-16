// Package core defines the provider-neutral payment contract that every
// provider adapter implements and the root client routes through.
package core

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Provider string

const (
	ProviderTino       Provider = "tino"
	ProviderQPay       Provider = "qpay"
	ProviderGolomt     Provider = "golomt"
	ProviderSocialPay  Provider = "socialpay"
	ProviderMonPay     Provider = "monpay"
	ProviderPocket     Provider = "pocket"
	ProviderBonum      Provider = "bonum"
	ProviderHiPay      Provider = "hipay"
	ProviderDigiPay    Provider = "digipay"
	ProviderArdPay     Provider = "ardpay"
	ProviderMongolChat Provider = "mongolchat"
	ProviderUPoint     Provider = "upoint"
	ProviderSimple     Provider = "simple"
	ProviderStorePay   Provider = "storepay"
	ProviderBalc       Provider = "balc"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusPaid      Status = "paid"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
	StatusRefunded  Status = "refunded"
)

type CreateInvoiceInput struct {
	Provider Provider
	// OrderID is the merchant-side unique reference for this payment.
	OrderID     string
	Amount      float64
	Description string
	// CallbackURL overrides the provider config's default callback for this invoice.
	CallbackURL string
	ReturnURL   string
	Phone       string
	CustomerID  string
	// ExpiresIn is honored only by providers that accept an invoice lifetime.
	ExpiresIn time.Duration
	// Extra carries provider-specific fields; each adapter documents the keys it reads.
	Extra map[string]string
}

type Invoice struct {
	Provider Provider `json:"provider"`
	// InvoiceID is the provider-side reference to pass back to CheckInvoice.
	InvoiceID  string     `json:"invoice_id"`
	OrderID    string     `json:"order_id"`
	Amount     float64    `json:"amount"`
	QRText     string     `json:"qr_text,omitempty"`
	QRImage    string     `json:"qr_image,omitempty"`
	PaymentURL string     `json:"payment_url,omitempty"`
	Deeplinks  []Deeplink `json:"deeplinks,omitempty"`
	Status     Status     `json:"status"`
	ExpiresAt  time.Time  `json:"expires_at,omitzero"`
	Raw        any        `json:"raw,omitempty"`
}

type Deeplink struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Logo        string `json:"logo,omitempty"`
	Link        string `json:"link"`
}

type CheckInvoiceInput struct {
	Provider  Provider
	InvoiceID string
	OrderID   string
	Amount    float64
}

type PaymentStatus struct {
	Provider      Provider `json:"provider"`
	InvoiceID     string   `json:"invoice_id"`
	Status        Status   `json:"status"`
	PaidAmount    float64  `json:"paid_amount"`
	TransactionID string   `json:"transaction_id,omitempty"`
	Raw           any      `json:"raw,omitempty"`
}

type CancelInvoiceInput struct {
	Provider  Provider
	InvoiceID string
	OrderID   string
	// Amount is required only by providers that sign cancel requests over it (SocialPay).
	Amount float64
}

type RefundInput struct {
	Provider      Provider
	InvoiceID     string
	OrderID       string
	TransactionID string
	// Amount of zero means a full refund.
	Amount float64
	Reason string
}

// Gateway is the contract every provider adapter implements.
type Gateway interface {
	Provider() Provider
	CreateInvoice(ctx context.Context, in CreateInvoiceInput) (*Invoice, error)
	CheckInvoice(ctx context.Context, in CheckInvoiceInput) (*PaymentStatus, error)
}

// Canceler is implemented by adapters whose provider can void an unpaid invoice.
type Canceler interface {
	CancelInvoice(ctx context.Context, in CancelInvoiceInput) error
}

// Refunder is implemented by adapters whose provider can return a captured payment.
type Refunder interface {
	Refund(ctx context.Context, in RefundInput) error
}

var (
	ErrNotConfigured = errors.New("payment: provider not configured")
	ErrUnsupported   = errors.New("payment: operation not supported by provider")
	ErrInvalidInput  = errors.New("payment: invalid input")
	ErrInvalidConfig = errors.New("payment: invalid provider config")
)

// ProviderError wraps a failure returned by a provider SDK or API.
type ProviderError struct {
	Provider Provider
	Op       string
	Err      error
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("payment: %s %s: %v", e.Provider, e.Op, e.Err)
}

func (e *ProviderError) Unwrap() error { return e.Err }

func Wrap(p Provider, op string, err error) error {
	if err == nil {
		return nil
	}
	return &ProviderError{Provider: p, Op: op, Err: err}
}

// RequireConfig returns ErrInvalidConfig naming every empty required field.
// fields alternates name, value: RequireConfig(p, "username", c.Username, ...).
func RequireConfig(p Provider, fields ...string) error {
	var missing []string
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i+1] == "" {
			missing = append(missing, fields[i])
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s missing %v", ErrInvalidConfig, p, missing)
}
