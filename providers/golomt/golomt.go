// Package golomt adapts Golomt Bank's e-commerce card checkout
// (github.com/techpartners-asia/golomt-api-go/ecommerce) to core.Gateway.
//
// CreateInvoice registers the order with Golomt and returns the hosted
// checkout page in PaymentURL. Golomt identifies the payment by the merchant's
// transactionId, which is the OrderID, so Invoice.InvoiceID equals OrderID.
// Golomt's own invoice number is part of PaymentURL and Raw.
//
// Extra keys read by CreateInvoice:
//   - return_type: GET (default), POST or MOBILE. How Golomt returns the
//     customer to the callback URL; MOBILE opens the app deeplink instead.
//   - lang: MN (default) or EN, the checkout page language.
//   - payment_method: payment (default, card form) or socialpay.
//   - get_token: "true" asks Golomt to tokenize the card once paid.
//   - social_deeplink: "true" asks for a SocialPay deeplink, returned in Deeplinks.
//
// CheckInvoice reports paid only on Golomt's success code "000". The SDK
// turns every other inquiry code into an error, so an unpaid invoice comes
// back as a *core.ProviderError rather than StatusPending.
//
// Refund is implemented with the SDK's void call (which the SDK marks as
// testing); it reverses the whole payment, so a non-zero Amount returns
// core.ErrUnsupported. Canceler is not implemented: Golomt has no call to
// void an unpaid invoice.
package golomt

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/techpartners-asia/golomt-api-go/ecommerce"
	"github.com/techpartners-asia/payment-all-in-one/core"
)

const DefaultBaseURL = "https://ecommerce.golomtbank.com"

const successCode = "000"

type Config struct {
	// BaseURL defaults to DefaultBaseURL. The hosted checkout page is served
	// from the same host.
	BaseURL     string
	Secret      string
	BearerToken string
	// CallbackURL is where Golomt returns the customer after checkout.
	CallbackURL string
}

type Adapter struct {
	client      ecommerce.GolomtEcommerce
	baseURL     string
	callbackURL string
}

var (
	_ core.Gateway  = (*Adapter)(nil)
	_ core.Refunder = (*Adapter)(nil)
)

func New(cfg Config) (*Adapter, error) {
	if err := core.RequireConfig(core.ProviderGolomt,
		"Secret", cfg.Secret,
		"BearerToken", cfg.BearerToken,
	); err != nil {
		return nil, err
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Adapter{
		client:      ecommerce.New(baseURL, cfg.Secret, cfg.BearerToken),
		baseURL:     baseURL,
		callbackURL: cfg.CallbackURL,
	}, nil
}

func (a *Adapter) Provider() core.Provider { return core.ProviderGolomt }

type checkoutOptions struct {
	returnType     ecommerce.ReturnType
	lang           ecommerce.Lang
	method         ecommerce.PaymentMethod
	getToken       bool
	socialDeeplink bool
}

func parseCheckoutOptions(extra map[string]string) (checkoutOptions, error) {
	opts := checkoutOptions{returnType: ecommerce.GET, lang: ecommerce.MN, method: ecommerce.EcommercePay}

	switch v := strings.ToUpper(extra["return_type"]); v {
	case "":
	case string(ecommerce.GET), string(ecommerce.POST), string(ecommerce.MOBILE):
		opts.returnType = ecommerce.ReturnType(v)
	default:
		return opts, fmt.Errorf("%w: golomt return_type %q, want GET, POST or MOBILE", core.ErrInvalidInput, extra["return_type"])
	}

	switch v := strings.ToUpper(extra["lang"]); v {
	case "":
	case string(ecommerce.MN), string(ecommerce.EN):
		opts.lang = ecommerce.Lang(v)
	default:
		return opts, fmt.Errorf("%w: golomt lang %q, want MN or EN", core.ErrInvalidInput, extra["lang"])
	}

	switch v := strings.ToLower(extra["payment_method"]); v {
	case "":
	case string(ecommerce.EcommercePay), string(ecommerce.SocialPay):
		opts.method = ecommerce.PaymentMethod(v)
	default:
		return opts, fmt.Errorf("%w: golomt payment_method %q, want payment or socialpay", core.ErrInvalidInput, extra["payment_method"])
	}

	var err error
	if opts.getToken, err = parseFlag(extra, "get_token"); err != nil {
		return opts, err
	}
	if opts.socialDeeplink, err = parseFlag(extra, "social_deeplink"); err != nil {
		return opts, err
	}
	return opts, nil
}

func parseFlag(extra map[string]string, key string) (bool, error) {
	v, ok := extra[key]
	if !ok || v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%w: golomt %s %q is not a boolean", core.ErrInvalidInput, key, v)
	}
	return b, nil
}

func (a *Adapter) CreateInvoice(_ context.Context, in core.CreateInvoiceInput) (*core.Invoice, error) {
	if in.OrderID == "" {
		return nil, fmt.Errorf("%w: golomt requires OrderID", core.ErrInvalidInput)
	}
	if in.Amount <= 0 {
		return nil, fmt.Errorf("%w: golomt requires a positive Amount", core.ErrInvalidInput)
	}
	callback := in.CallbackURL
	if callback == "" {
		callback = a.callbackURL
	}
	if callback == "" {
		return nil, fmt.Errorf("%w: golomt requires a CallbackURL", core.ErrInvalidInput)
	}
	opts, err := parseCheckoutOptions(in.Extra)
	if err != nil {
		return nil, err
	}

	res, err := a.client.CreateInvoice(ecommerce.CreateInvoiceInput{
		Amount:         in.Amount,
		TransactionID:  in.OrderID,
		ReturnType:     opts.returnType,
		Callback:       callback,
		GetToken:       opts.getToken,
		SocialDeeplink: opts.socialDeeplink,
	})
	if err != nil {
		return nil, core.Wrap(core.ProviderGolomt, "create_invoice", err)
	}
	// The SDK skips the errorCode check on create, so a rejected request
	// arrives here as a response without an invoice number.
	if res.Invoice == "" {
		return nil, core.Wrap(core.ProviderGolomt, "create_invoice",
			fmt.Errorf("no invoice returned: code %q: %s", res.ErrorCode, res.ErrorDesc))
	}

	inv := &core.Invoice{
		Provider:   core.ProviderGolomt,
		InvoiceID:  in.OrderID,
		OrderID:    in.OrderID,
		Amount:     in.Amount,
		PaymentURL: a.checkoutURL(opts, res.Invoice),
		Status:     core.StatusPending,
		Raw:        res,
	}
	if res.SocialDeeplink != "" {
		inv.Deeplinks = []core.Deeplink{{Name: "SocialPay", Link: res.SocialDeeplink}}
	}
	return inv, nil
}

// checkoutURL mirrors ecommerce.GetInvoiceUrl, which hardcodes the production
// host; building it from baseURL keeps a sandbox BaseURL on the sandbox page.
func (a *Adapter) checkoutURL(opts checkoutOptions, invoice string) string {
	return fmt.Sprintf("%s/%s/%s/%s", a.baseURL, opts.method, opts.lang, invoice)
}

func (a *Adapter) CheckInvoice(_ context.Context, in core.CheckInvoiceInput) (*core.PaymentStatus, error) {
	txnID := transactionID(in.InvoiceID, in.OrderID)
	if txnID == "" {
		return nil, fmt.Errorf("%w: golomt check requires InvoiceID or OrderID", core.ErrInvalidInput)
	}
	res, err := a.client.Inquiry(txnID)
	if err != nil {
		return nil, core.Wrap(core.ProviderGolomt, "check_invoice", err)
	}
	paid, err := strconv.ParseFloat(res.Amount, 64)
	if err != nil {
		return nil, core.Wrap(core.ProviderGolomt, "check_invoice", fmt.Errorf("amount %q: %w", res.Amount, err))
	}
	return &core.PaymentStatus{
		Provider:      core.ProviderGolomt,
		InvoiceID:     txnID,
		Status:        inquiryStatus(res.ErrorCode),
		PaidAmount:    paid,
		TransactionID: res.TransactionID,
		Raw:           res,
	}, nil
}

func inquiryStatus(errorCode string) core.Status {
	if errorCode == successCode {
		return core.StatusPaid
	}
	return core.StatusPending
}

func (a *Adapter) Refund(_ context.Context, in core.RefundInput) error {
	txnID := transactionID(in.InvoiceID, in.OrderID)
	if txnID == "" {
		return fmt.Errorf("%w: golomt refund requires InvoiceID or OrderID", core.ErrInvalidInput)
	}
	if in.Amount != 0 {
		return core.Wrap(core.ProviderGolomt, "refund",
			fmt.Errorf("%w: golomt voids the whole payment, partial refunds are not available", core.ErrUnsupported))
	}
	if _, err := a.client.Refund(txnID); err != nil {
		return core.Wrap(core.ProviderGolomt, "refund", err)
	}
	return nil
}

func transactionID(invoiceID, orderID string) string {
	if invoiceID != "" {
		return invoiceID
	}
	return orderID
}
