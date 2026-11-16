// Package mongolchat adapts MongolChat's online QR API
// (github.com/techpartners-asia/mongolchat-go/mongolchat) to core.Gateway.
//
// CreateInvoice generates an online QR (worker/onlineqr/generate) and returns
// the QR string as both InvoiceID and QRText, because worker/onlineqr/status
// looks payments up by that string. The status response carries no amount, so
// PaymentStatus.PaidAmount stays zero. Refund calls worker/transaction/refund
// with RefundInput.TransactionID (the id CheckInvoice reports) and needs an
// explicit Amount. There is no cancel operation. MongolChat delivers payment
// webhooks to a URL registered with MongolChat, so CallbackURL is unused.
//
// Extra keys read by CreateInvoice:
//   - "branch_id": overrides Config.BranchID.
//   - "sub_title": QR sub title.
//   - "noat", "nhat", "ttd": VAT, city tax and taxpayer number for the receipt.
//   - "expire_time": passed through verbatim in MongolChat's format.
package mongolchat

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/techpartners-asia/mongolchat-go/mongolchat"
	"github.com/techpartners-asia/payment-all-in-one/core"
)

type Config struct {
	// Endpoint is the API base URL, without a trailing slash.
	Endpoint string
	// APIKey is sent as the api-key header.
	APIKey string
	// WorkerKey is sent as "Authorization: WorkerKey <key>".
	WorkerKey string
	// BranchID is the default branch_id for generated QRs.
	BranchID string
}

type Adapter struct {
	branchID string
	client   mongolchat.MongolChat
}

var (
	_ core.Gateway  = (*Adapter)(nil)
	_ core.Refunder = (*Adapter)(nil)
)

func New(cfg Config) (*Adapter, error) {
	if err := core.RequireConfig(core.ProviderMongolChat,
		"Endpoint", cfg.Endpoint,
		"APIKey", cfg.APIKey,
		"WorkerKey", cfg.WorkerKey,
	); err != nil {
		return nil, err
	}
	return &Adapter{
		branchID: cfg.BranchID,
		// The SDK stores but never uses appsecret and branchno.
		client: mongolchat.New(strings.TrimRight(cfg.Endpoint, "/"), cfg.APIKey, cfg.WorkerKey, "", cfg.BranchID),
	}, nil
}

func (a *Adapter) Provider() core.Provider { return core.ProviderMongolChat }

func (a *Adapter) CreateInvoice(_ context.Context, in core.CreateInvoiceInput) (*core.Invoice, error) {
	if err := a.InvoiceRules().Validate(a.Provider(), in); err != nil {
		return nil, err
	}
	if in.OrderID == "" {
		return nil, fmt.Errorf("%w: mongolchat order id is required", core.ErrInvalidInput)
	}
	amount, err := wholeAmount(in.Amount)
	if err != nil {
		return nil, err
	}
	title := in.Description
	if title == "" {
		title = in.OrderID
	}
	branchID := a.branchID
	if b := in.Extra["branch_id"]; b != "" {
		branchID = b
	}

	req := mongolchat.MchatOnlineQrGenerateRequest{
		Amount:          amount,
		BranchID:        branchID,
		Products:        []mongolchat.MchatProduct{{ProductName: title, Quantity: "1", Price: amount}},
		Title:           title,
		SubTitle:        in.Extra["sub_title"],
		NOAT:            in.Extra["noat"],
		NHAT:            in.Extra["nhat"],
		TTD:             in.Extra["ttd"],
		ReferenceNumber: in.OrderID,
		ExpireTime:      in.Extra["expire_time"],
	}
	res, err := call(func() (mongolchat.MchatOnlineQrGenerateResponse, error) { return a.client.GenerateQR(req) })
	if err != nil {
		return nil, core.Wrap(core.ProviderMongolChat, "create_invoice", err)
	}
	if res.Qr == "" {
		return nil, core.Wrap(core.ProviderMongolChat, "create_invoice", errors.New("response carried no qr"))
	}

	return &core.Invoice{
		Provider:  core.ProviderMongolChat,
		InvoiceID: res.Qr,
		OrderID:   in.OrderID,
		Amount:    in.Amount,
		QRText:    res.Qr,
		Status:    core.StatusPending,
		Raw:       res,
	}, nil
}

func (a *Adapter) CheckInvoice(_ context.Context, in core.CheckInvoiceInput) (*core.PaymentStatus, error) {
	if in.InvoiceID == "" {
		return nil, fmt.Errorf("%w: mongolchat qr is required as invoice id", core.ErrInvalidInput)
	}
	res, err := call(func() (mongolchat.MchatOnlineQrCheckResponse, error) { return a.client.CheckQR(in.InvoiceID) })
	if err != nil {
		return nil, core.Wrap(core.ProviderMongolChat, "check_invoice", err)
	}
	st := &core.PaymentStatus{
		Provider:  core.ProviderMongolChat,
		InvoiceID: in.InvoiceID,
		Status:    qrStatus(res.Status),
		Raw:       res,
	}
	if st.Status == core.StatusPaid {
		st.TransactionID = res.ID
	}
	return st, nil
}

func (a *Adapter) Refund(_ context.Context, in core.RefundInput) error {
	if in.TransactionID == "" {
		return fmt.Errorf("%w: mongolchat refund needs the transaction id", core.ErrInvalidInput)
	}
	if in.Amount <= 0 {
		return fmt.Errorf("%w: mongolchat refund needs an explicit amount", core.ErrInvalidInput)
	}
	req := mongolchat.MchatTransactionRefundRequest{ID: in.TransactionID, Amount: in.Amount, Reason: in.Reason}
	_, err := call(func() (mongolchat.MchatTransactionRefundResponse, error) { return a.client.RefundTransaction(req) })
	return core.Wrap(core.ProviderMongolChat, "refund", err)
}

// qrStatus maps onlineqr/status. "paid" is the only status the SDK documents;
// anything else is treated as still awaiting payment.
func qrStatus(s string) core.Status {
	if strings.EqualFold(s, "paid") {
		return core.StatusPaid
	}
	return core.StatusPending
}

// call converts SDK panics into errors: on a transport failure the SDK
// dereferences a nil *http.Response.
func call[T any](fn func() (T, error)) (res T, err error) {
	defer func() {
		if r := recover(); r != nil {
			var zero T
			res, err = zero, fmt.Errorf("mongolchat sdk panic: %v", r)
		}
	}()
	return fn()
}

func wholeAmount(amount float64) (int, error) {
	if amount <= 0 || amount != math.Trunc(amount) || amount > math.MaxInt32 {
		return 0, fmt.Errorf("%w: mongolchat amount must be a positive whole number, got %v", core.ErrInvalidInput, amount)
	}
	return int(amount), nil
}
