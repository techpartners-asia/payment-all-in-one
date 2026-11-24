// Package qpay adapts the QPay V2 merchant invoice API
// (github.com/techpartners-asia/qpay-go/qpay_v2) to core.Gateway.
//
// CreateInvoice issues a QPay invoice and returns its QR, short URL and bank
// app deeplinks. CheckInvoice runs payment/check against the invoice id.
// CancelInvoice voids an unpaid invoice. Refund returns a paid payment in full
// and needs RefundInput.TransactionID (the payment id CheckInvoice reports).
//
// The adapter logs in once and reuses the access token until shortly before it
// expires. It then renews through /auth/refresh while the refresh token is
// live, and logs in again only when that is not possible.
//
// Extra keys read by CreateInvoice:
//   - "invoice_receiver_code": receiver code when CustomerID is empty (default "terminal").
//   - "sender_branch_code": merchant branch code.
//   - "note": invoice note.
package qpay

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/techpartners-asia/payment-all-in-one/core"
	"github.com/techpartners-asia/qpay-go/qpay_v2"
)

type Config struct {
	Username string
	Password string
	// Endpoint is the V2 base URL, e.g. https://merchant.qpay.mn/v2.
	Endpoint    string
	InvoiceCode string
	MerchantID  string
	CallbackURL string
}

type Adapter struct {
	client      qpay_v2.QPay
	callbackURL string

	// tokenMu serializes renewals so concurrent calls with an expired token
	// share one /auth/refresh or /auth/token round trip.
	tokenMu sync.Mutex
}

var (
	_ core.Gateway  = (*Adapter)(nil)
	_ core.Canceler = (*Adapter)(nil)
	_ core.Refunder = (*Adapter)(nil)
)

// ulaanbaatar is fixed because Mongolia has had no DST since 2017 and
// time.LoadLocation depends on tzdata being present on the host.
var ulaanbaatar = time.FixedZone("ULAT", 8*60*60)

const tokenRefreshMargin = 30 * time.Second

func New(cfg Config) (*Adapter, error) {
	if err := core.RequireConfig(core.ProviderQPay,
		"Username", cfg.Username,
		"Password", cfg.Password,
		"Endpoint", cfg.Endpoint,
		"InvoiceCode", cfg.InvoiceCode,
	); err != nil {
		return nil, err
	}
	return &Adapter{
		client:      qpay_v2.New(cfg.Username, cfg.Password, cfg.Endpoint, cfg.CallbackURL, cfg.InvoiceCode, cfg.MerchantID),
		callbackURL: cfg.CallbackURL,
	}, nil
}

func (a *Adapter) Provider() core.Provider { return core.ProviderQPay }

func (a *Adapter) CreateInvoice(ctx context.Context, in core.CreateInvoiceInput) (*core.Invoice, error) {
	if err := a.InvoiceRules().Validate(a.Provider(), in); err != nil {
		return nil, err
	}
	if in.OrderID == "" {
		return nil, fmt.Errorf("%w: qpay order id is required", core.ErrInvalidInput)
	}
	amount, err := wholeAmount(in.Amount)
	if err != nil {
		return nil, err
	}
	callbackURL := a.callbackURL
	if in.CallbackURL != "" {
		callbackURL = in.CallbackURL
	}
	if callbackURL == "" {
		return nil, fmt.Errorf("%w: qpay callback url is required", core.ErrInvalidInput)
	}

	req := qpay_v2.QPayCreateInvoiceInput{
		SenderInvoiceNo:     in.OrderID,
		SenderBranchCode:    in.Extra["sender_branch_code"],
		InvoiceReceiverCode: receiverCode(in),
		InvoiceDescription:  in.Description,
		Amount:              amount,
		CallbackUrl:         &callbackURL,
		Note:                in.Extra["note"],
	}
	if req.InvoiceDescription == "" {
		req.InvoiceDescription = in.OrderID
	}
	if in.Phone != "" {
		req.InvoiceReceiverData = &qpay_v2.InvoiceReceiverData{Phone: in.Phone}
	}
	var expiresAt time.Time
	if in.ExpiresIn > 0 {
		expiresAt = time.Now().Add(in.ExpiresIn).In(ulaanbaatar).Truncate(time.Second)
		req.EnableExpiry = true
		req.ExpiryDate = expiresAt.Format(time.DateTime)
	}

	res, err := withToken(ctx, a, func() (qpay_v2.QPaySimpleInvoiceResponse, error) {
		return a.client.CreateInvoice(req)
	})
	if err != nil {
		return nil, core.Wrap(core.ProviderQPay, "create_invoice", err)
	}

	return &core.Invoice{
		Provider:   core.ProviderQPay,
		InvoiceID:  res.InvoiceID,
		OrderID:    in.OrderID,
		Amount:     in.Amount,
		QRText:     res.QrText,
		QRImage:    res.QrImage,
		PaymentURL: res.QpayShortUrl,
		Deeplinks:  deeplinks(res.Urls),
		Status:     core.StatusPending,
		ExpiresAt:  expiresAt,
		Raw:        res,
	}, nil
}

func (a *Adapter) CheckInvoice(ctx context.Context, in core.CheckInvoiceInput) (*core.PaymentStatus, error) {
	if in.InvoiceID == "" {
		return nil, fmt.Errorf("%w: qpay invoice id is required", core.ErrInvalidInput)
	}
	res, err := withToken(ctx, a, func() (qpay_v2.QpayPaymentCheckResponse, error) {
		return a.client.CheckPayment(in.InvoiceID, 100, 1)
	})
	if err != nil {
		return nil, core.Wrap(core.ProviderQPay, "check_invoice", err)
	}

	st := summarizePayments(res.Rows)
	if st.status == core.StatusPaid && in.Amount > 0 && st.paidAmount < in.Amount {
		st.status = core.StatusPending
	}
	return &core.PaymentStatus{
		Provider:      core.ProviderQPay,
		InvoiceID:     in.InvoiceID,
		Status:        st.status,
		PaidAmount:    st.paidAmount,
		TransactionID: st.transactionID,
		Raw:           res,
	}, nil
}

func (a *Adapter) CancelInvoice(ctx context.Context, in core.CancelInvoiceInput) error {
	if in.InvoiceID == "" {
		return fmt.Errorf("%w: qpay invoice id is required", core.ErrInvalidInput)
	}
	_, err := withToken(ctx, a, func() (qpay_v2.QpayGeneralResponse, error) {
		return a.client.CancelInvoice(in.InvoiceID)
	})
	return core.Wrap(core.ProviderQPay, "cancel_invoice", err)
}

// Refund returns the whole payment: QPay's refund endpoint takes no amount.
func (a *Adapter) Refund(ctx context.Context, in core.RefundInput) error {
	if in.TransactionID == "" {
		return fmt.Errorf("%w: qpay refund needs the payment id as transaction id", core.ErrInvalidInput)
	}
	if in.Amount != 0 {
		return fmt.Errorf("%w: qpay refunds the full payment only; pass amount 0", core.ErrInvalidInput)
	}
	_, err := withToken(ctx, a, func() (qpay_v2.QpayGeneralResponse, error) {
		return a.client.RefundPayment(in.InvoiceID, in.TransactionID)
	})
	return core.Wrap(core.ProviderQPay, "refund", err)
}

// withToken runs call with a valid access token installed, logging in again
// once if QPay rejects the token. A rejected request was never processed by
// QPay, so the retry cannot double-create an invoice.
func withToken[T any](ctx context.Context, a *Adapter, call func() (T, error)) (T, error) {
	var zero T
	used, err := a.ensureToken(ctx, "")
	if err != nil {
		return zero, err
	}
	res, err := call()
	if !errors.Is(err, qpay_v2.ErrUnauthorized) {
		return res, err
	}
	if _, err := a.ensureToken(ctx, used); err != nil {
		return zero, err
	}
	return call()
}

// ensureToken returns the access token now installed, renewing it when there
// is none, it is about to expire, or it equals rejected.
func (a *Adapter) ensureToken(ctx context.Context, rejected string) (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()

	cur := a.client.Token()
	now := time.Now()
	if !cur.IsZero() && cur.AccessToken != rejected && now.Add(tokenRefreshMargin).Before(cur.ExpiresAt) {
		return cur.AccessToken, nil
	}
	tok, err := a.renew(ctx, cur, now)
	if err != nil {
		return "", err
	}
	a.client.SetToken(tok)
	return tok.AccessToken, nil
}

// renew trades a live refresh token for a new access token, and logs in with
// the merchant credentials only when there is no usable refresh token or
// QPay refuses it.
func (a *Adapter) renew(ctx context.Context, cur qpay_v2.Token, now time.Time) (qpay_v2.Token, error) {
	if cur.RefreshToken != "" && now.Add(tokenRefreshMargin).Before(cur.RefreshExpiresAt) {
		if tok, err := a.client.Refresh(ctx, cur.RefreshToken); err == nil {
			return tok, nil
		}
	}
	return a.client.Login(ctx)
}

type paymentSummary struct {
	status        core.Status
	paidAmount    float64
	transactionID string
}

func paymentRowStatus(s string) core.Status {
	switch s {
	case "PAID":
		return core.StatusPaid
	case "REFUNDED":
		return core.StatusRefunded
	case "FAILED":
		return core.StatusFailed
	default:
		return core.StatusPending
	}
}

// summarizePayments folds payment/check rows into one status. A failed
// attempt leaves the invoice payable, so the invoice reads failed only when
// every row failed.
func summarizePayments(rows []*qpay_v2.QpayRow) paymentSummary {
	var sum paymentSummary
	seen := map[core.Status]bool{}
	for _, row := range rows {
		if row == nil {
			continue
		}
		status := paymentRowStatus(row.PaymentStatus)
		seen[status] = true
		if status != core.StatusPaid {
			continue
		}
		if amount, err := strconv.ParseFloat(row.PaymentAmount.String(), 64); err == nil {
			sum.paidAmount += amount
		}
		if sum.transactionID == "" {
			sum.transactionID = row.PaymentID
		}
	}
	switch {
	case seen[core.StatusPaid]:
		sum.status = core.StatusPaid
	case seen[core.StatusPending]:
		sum.status = core.StatusPending
	case seen[core.StatusRefunded]:
		sum.status = core.StatusRefunded
	case seen[core.StatusFailed]:
		sum.status = core.StatusFailed
	default:
		sum.status = core.StatusPending
	}
	return sum
}

func receiverCode(in core.CreateInvoiceInput) string {
	if in.CustomerID != "" {
		return in.CustomerID
	}
	if code := in.Extra["invoice_receiver_code"]; code != "" {
		return code
	}
	return "terminal"
}

func deeplinks(urls []*qpay_v2.Deeplink) []core.Deeplink {
	out := make([]core.Deeplink, 0, len(urls))
	for _, u := range urls {
		if u == nil || u.Link == "" {
			continue
		}
		out = append(out, core.Deeplink{Name: u.Name, Description: u.Description, Logo: u.Logo, Link: u.Link})
	}
	return out
}

// wholeAmount converts to QPay's integer tugrik amount, refusing fractions
// rather than silently truncating them.
func wholeAmount(amount float64) (int64, error) {
	if amount <= 0 || amount != math.Trunc(amount) || amount > math.MaxInt64 {
		return 0, fmt.Errorf("%w: qpay amount must be a positive whole number, got %v", core.ErrInvalidInput, amount)
	}
	return int64(amount), nil
}
