// Package core defines the provider-neutral payment contract that every
// provider adapter implements and the root client routes through.
package core

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
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
	// InvoiceRules may depend on the adapter's config (e.g. a callback URL
	// is required per call only when the config has none).
	InvoiceRules() InvoiceRules
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

// Field names an input of CreateInvoiceInput. Extra keys are written
// ExtraField("card_number").
type Field string

const (
	FieldOrderID     Field = "OrderID"
	FieldAmount      Field = "Amount"
	FieldDescription Field = "Description"
	FieldCallbackURL Field = "CallbackURL"
	FieldReturnURL   Field = "ReturnURL"
	FieldPhone       Field = "Phone"
	FieldCustomerID  Field = "CustomerID"
)

func ExtraField(key string) Field { return Field("Extra." + key) }

// Value returns the field's value from in, or "" when unset.
func (in CreateInvoiceInput) Value(f Field) string {
	switch f {
	case FieldOrderID:
		return in.OrderID
	case FieldAmount:
		if in.Amount == 0 {
			return ""
		}
		return fmt.Sprint(in.Amount)
	case FieldDescription:
		return in.Description
	case FieldCallbackURL:
		return in.CallbackURL
	case FieldReturnURL:
		return in.ReturnURL
	case FieldPhone:
		return in.Phone
	case FieldCustomerID:
		return in.CustomerID
	}
	if key, ok := strings.CutPrefix(string(f), "Extra."); ok {
		return in.Extra[key]
	}
	return ""
}

// InvoiceRules declares what a provider's CreateInvoice accepts, so callers
// can validate before any network call and UIs can ask for the right fields.
type InvoiceRules struct {
	// Required fields must be non-empty. OrderID and Amount are always required.
	Required []Field
	// AnyOf groups need at least one non-empty field each.
	AnyOf [][]Field
	// WholeAmount means the provider accepts only whole tugrik.
	WholeAmount bool
	// Extra lists every Extra key the provider reads. Any other key is
	// rejected so a typo cannot be silently ignored.
	Extra map[string]ExtraRule
	// Check covers requirements that depend on other fields. Its error joins
	// the others, so it should wrap ErrInvalidInput.
	Check func(CreateInvoiceInput) error
}

type ExtraRule struct {
	Doc string
	// Values, when set, are the only accepted values, compared case-insensitively.
	Values []string
	// Number requires a non-negative decimal number.
	Number bool
}

// Validate reports every rule in violates, joined, each wrapping ErrInvalidInput.
func (r InvoiceRules) Validate(p Provider, in CreateInvoiceInput) error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%w: %s: "+format, append([]any{ErrInvalidInput, p}, args...)...))
	}
	if in.OrderID == "" {
		fail("OrderID is required")
	}
	switch {
	case math.IsNaN(in.Amount) || math.IsInf(in.Amount, 0) || in.Amount <= 0:
		fail("Amount must be positive, got %v", in.Amount)
	case r.WholeAmount && in.Amount != math.Trunc(in.Amount):
		fail("Amount must be a whole number of tugrik, got %v", in.Amount)
	}
	for _, f := range r.Required {
		if in.Value(f) == "" {
			fail("%s is required", f)
		}
	}
	for _, group := range r.AnyOf {
		if !slices.ContainsFunc(group, func(f Field) bool { return in.Value(f) != "" }) {
			fail("one of %v is required", group)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(in.Extra)) {
		rule, ok := r.Extra[key]
		v := in.Extra[key]
		switch {
		case !ok:
			fail("unknown Extra key %q (accepted: %v)", key, slices.Sorted(maps.Keys(r.Extra)))
		case v == "":
		case len(rule.Values) > 0 && !slices.ContainsFunc(rule.Values, func(want string) bool { return strings.EqualFold(want, v) }):
			fail("Extra[%q] = %q, want one of %v", key, v, rule.Values)
		case rule.Number && !isNonNegativeNumber(v):
			fail("Extra[%q] = %q, want a non-negative number", key, v)
		}
	}
	if r.Check != nil {
		if err := r.Check(in); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func isNonNegativeNumber(s string) bool {
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f >= 0 && !math.IsInf(f, 0)
}
