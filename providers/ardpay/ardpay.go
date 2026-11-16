// Package ardpay adapts the ArdPay merchant QR API
// (github.com/techpartners-asia/ardpay-go) to core.Gateway.
//
// CreateInvoice calls createqr with OrderID as the ArdPay invoiceId. ArdPay
// identifies the payment by the returned qrCode afterwards (checkqr and
// cancelqr both take it), so Invoice.InvoiceID is that qrCode, and it is
// also the QR text to render.
//
// The SDK takes no context, so ctx is ignored.
//
// Extra keys:
//   - paid_limit (optional, CreateInvoice): value for the createqr paidLimit
//     field, a decimal number. Omitted means 0.
//
// Optional operations: core.Canceler (cancelqr). The SDK exposes no refund.
package ardpay

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	sdk "github.com/techpartners-asia/ardpay-go"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

type Config struct {
	// BaseURL is the ArdPay host; the SDK appends /resources/merch/v1.0/....
	BaseURL string
	// MerchantID is sent as payeeCode.
	MerchantID string
	PosNo      string
	APIKey     string
	// Currency defaults to MNT.
	Currency string
}

type Adapter struct {
	client   sdk.Ardpay
	currency string
}

var (
	_ core.Gateway  = (*Adapter)(nil)
	_ core.Canceler = (*Adapter)(nil)
)

func New(cfg Config) (*Adapter, error) {
	if err := core.RequireConfig(core.ProviderArdPay,
		"BaseURL", cfg.BaseURL,
		"MerchantID", cfg.MerchantID,
		"PosNo", cfg.PosNo,
		"APIKey", cfg.APIKey,
	); err != nil {
		return nil, err
	}
	currency := cfg.Currency
	if currency == "" {
		currency = "MNT"
	}
	return &Adapter{
		client:   sdk.New(cfg.BaseURL, cfg.MerchantID, cfg.PosNo, cfg.APIKey),
		currency: currency,
	}, nil
}

func (a *Adapter) Provider() core.Provider { return core.ProviderArdPay }

func (a *Adapter) CreateInvoice(_ context.Context, in core.CreateInvoiceInput) (*core.Invoice, error) {
	if err := a.InvoiceRules().Validate(a.Provider(), in); err != nil {
		return nil, err
	}
	if in.OrderID == "" {
		return nil, fmt.Errorf("%w: OrderID is required", core.ErrInvalidInput)
	}
	if in.Amount <= 0 {
		return nil, fmt.Errorf("%w: amount must be positive", core.ErrInvalidInput)
	}
	var paidLimit float64
	if s := in.Extra["paid_limit"]; s != "" {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: Extra[\"paid_limit\"] %q is not a number", core.ErrInvalidInput, s)
		}
		paidLimit = v
	}

	res, err := a.client.CreateQrPayment(sdk.CreateQrInput{
		Amount:      in.Amount,
		Currency:    a.currency,
		Description: in.Description,
		InvoiceID:   in.OrderID,
		PaidLimit:   paidLimit,
	})
	if err != nil {
		return nil, core.Wrap(core.ProviderArdPay, "create_invoice", err)
	}
	// The SDK silently skips decoding a body not labeled as JSON, which would
	// otherwise pass here as a success with no QR.
	if res.QrCode == "" {
		return nil, core.Wrap(core.ProviderArdPay, "create_invoice", errors.New("response has no qrCode"))
	}
	return &core.Invoice{
		Provider:   core.ProviderArdPay,
		InvoiceID:  res.QrCode,
		OrderID:    in.OrderID,
		Amount:     in.Amount,
		QRText:     res.QrCode,
		PaymentURL: res.QrLink,
		Status:     core.StatusPending,
		Raw:        res,
	}, nil
}

func (a *Adapter) CheckInvoice(_ context.Context, in core.CheckInvoiceInput) (*core.PaymentStatus, error) {
	if in.InvoiceID == "" {
		return nil, fmt.Errorf("%w: InvoiceID (the ArdPay qrCode) is required", core.ErrInvalidInput)
	}
	res, err := a.client.CheckQr(sdk.CheckQrInput{QrCode: []string{in.InvoiceID}})
	if err != nil {
		return nil, core.Wrap(core.ProviderArdPay, "check_invoice", err)
	}
	ps := &core.PaymentStatus{
		Provider:  core.ProviderArdPay,
		InvoiceID: in.InvoiceID,
		Status:    core.StatusPending,
		Raw:       res,
	}
	if item, ok := findQr(res.QrList, in.InvoiceID); ok {
		ps.Status = mapStatus(item)
		if ps.Status == core.StatusPaid {
			ps.PaidAmount = float64(item.TranAmnt)
			ps.TransactionID = item.TranId
		}
	}
	return ps, nil
}

func (a *Adapter) CancelInvoice(_ context.Context, in core.CancelInvoiceInput) error {
	if in.InvoiceID == "" {
		return fmt.Errorf("%w: InvoiceID (the ArdPay qrCode) is required", core.ErrInvalidInput)
	}
	res, err := a.client.CancelQrPayment(sdk.CancelQrInput{QrCodes: []string{in.InvoiceID}})
	if err != nil {
		return core.Wrap(core.ProviderArdPay, "cancel_invoice", err)
	}
	item, ok := findQr(res.Qrlist, in.InvoiceID)
	if !ok {
		return core.Wrap(core.ProviderArdPay, "cancel_invoice", errors.New("qr missing from cancel response"))
	}
	if item.ResponseCode != 0 {
		return core.Wrap(core.ProviderArdPay, "cancel_invoice",
			fmt.Errorf("qr response code %d: %s", item.ResponseCode, item.ResponseDesc))
	}
	return nil
}

func findQr(list []sdk.QrList, qrCode string) (sdk.QrList, bool) {
	for _, item := range list {
		if item.QrCode == qrCode {
			return item, true
		}
	}
	return sdk.QrList{}, false
}

// mapStatus treats a QR as paid only when ArdPay reports success for it and
// names the transaction; the API documents no other per-QR states.
func mapStatus(item sdk.QrList) core.Status {
	if item.ResponseCode == 0 && (item.TranId != "" || item.PaymentId != 0) {
		return core.StatusPaid
	}
	return core.StatusPending
}
