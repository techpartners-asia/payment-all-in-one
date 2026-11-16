// Package monpay adapts MonPay's merchant branch QR purchase API
// (github.com/techpartners-asia/monpay-go/monpay, monpay.New) to
// core.Gateway.
//
// CreateInvoice generates a one-time purchase QR (qrpurchase/generate) and
// CheckInvoice polls it by UUID (qrpurchase/check). The branch QR API has no
// cancel or refund, so the adapter implements neither. It reads no Extra keys
// and ignores ExpiresIn, OrderID and Description beyond echoing OrderID back,
// because the QR request carries only amount and callback URL.
//
// MonPay's Mini App invoice API (monpay.NewDeeplink) is a separate product
// that only works inside the MonPay app and is not wired here.
package monpay

import (
	"context"
	"fmt"
	"math"

	"github.com/techpartners-asia/monpay-go/monpay"
	"github.com/techpartners-asia/payment-all-in-one/core"
)

type Config struct {
	// Endpoint is the wallet base URL, e.g. https://z-wallet.monpay.mn.
	Endpoint string
	// Username is the branch username, sent as the basic-auth user.
	Username string
	// AccountID is the branch account id, sent as the basic-auth password.
	AccountID   string
	CallbackURL string
}

type Adapter struct {
	cfg    Config
	client monpay.Monpay
}

var _ core.Gateway = (*Adapter)(nil)

// Response codes from qrpurchase/check.
const (
	codeOK         = 0
	codeNotScanned = 23
)

func New(cfg Config) (*Adapter, error) {
	if err := core.RequireConfig(core.ProviderMonPay,
		"Endpoint", cfg.Endpoint,
		"Username", cfg.Username,
		"AccountID", cfg.AccountID,
	); err != nil {
		return nil, err
	}
	return &Adapter{
		cfg:    cfg,
		client: monpay.New(cfg.Endpoint, cfg.Username, cfg.AccountID, cfg.CallbackURL),
	}, nil
}

func (a *Adapter) Provider() core.Provider { return core.ProviderMonPay }

func (a *Adapter) CreateInvoice(_ context.Context, in core.CreateInvoiceInput) (*core.Invoice, error) {
	if in.Amount <= 0 || math.IsInf(in.Amount, 0) || math.IsNaN(in.Amount) {
		return nil, fmt.Errorf("%w: monpay amount must be positive, got %v", core.ErrInvalidInput, in.Amount)
	}

	client := a.client
	if in.CallbackURL != "" && in.CallbackURL != a.cfg.CallbackURL {
		// The SDK binds the callback URL at construction and GenerateQr takes
		// no override, so a per-invoice callback needs its own client.
		client = monpay.New(a.cfg.Endpoint, a.cfg.Username, a.cfg.AccountID, in.CallbackURL)
	}

	res, err := client.GenerateQr(monpay.MonpayQrInput{Amount: in.Amount})
	if err != nil {
		return nil, core.Wrap(core.ProviderMonPay, "create_invoice", qrError(err, res.Code, res.Info))
	}
	if res.Result.UUID == "" {
		return nil, core.Wrap(core.ProviderMonPay, "create_invoice", fmt.Errorf("response carried no qr uuid"))
	}

	return &core.Invoice{
		Provider:  core.ProviderMonPay,
		InvoiceID: res.Result.UUID,
		OrderID:   in.OrderID,
		Amount:    in.Amount,
		QRText:    res.Result.Qrcode,
		Status:    core.StatusPending,
		Raw:       res,
	}, nil
}

func (a *Adapter) CheckInvoice(_ context.Context, in core.CheckInvoiceInput) (*core.PaymentStatus, error) {
	if in.InvoiceID == "" {
		return nil, fmt.Errorf("%w: monpay qr uuid is required as invoice id", core.ErrInvalidInput)
	}
	res, err := a.client.CheckQr(in.InvoiceID)
	// The SDK reports "QR not scanned yet" as an error alongside a decoded
	// response; that is the normal unpaid state, not a failure.
	if err != nil && res.Code != codeNotScanned {
		return nil, core.Wrap(core.ProviderMonPay, "check_invoice", qrError(err, res.Code, res.Info))
	}

	st := &core.PaymentStatus{
		Provider:  core.ProviderMonPay,
		InvoiceID: in.InvoiceID,
		Status:    checkStatus(res),
		Raw:       res,
	}
	if st.Status == core.StatusPaid {
		st.PaidAmount = float64(res.Result.Amount)
		st.TransactionID = res.Result.TransactionId
	}
	return st, nil
}

// checkStatus treats a QR as paid only when MonPay reports it used, so an
// unexpected success body without payment evidence stays pending.
func checkStatus(res monpay.MonpayCheckResponse) core.Status {
	if res.Code == codeOK && (res.Result.UsedAt != 0 || res.Result.TransactionId != "") {
		return core.StatusPaid
	}
	return core.StatusPending
}

// qrError keeps MonPay's code and info, which the SDK's own errors drop.
func qrError(err error, code int, info string) error {
	if code == codeOK && info == "" {
		return err
	}
	return fmt.Errorf("%w (code=%d info=%q)", err, code, info)
}
