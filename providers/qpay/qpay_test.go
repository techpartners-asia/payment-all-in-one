package qpay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/techpartners-asia/payment-all-in-one/core"
	"github.com/techpartners-asia/qpay-go/qpay_v2"
)

type fakeQPay struct {
	logins       atomic.Int32
	rejectNext   atomic.Bool
	lastInvoice  map[string]any
	checkBody    string
	lastDeletion string
}

func (f *fakeQPay) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/token", func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if user != "user" || pass != "pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n := f.logins.Add(1)
		writeJSON(w, map[string]any{
			"token_type":         "bearer",
			"access_token":       "tok-" + string(rune('0'+n)),
			"refresh_token":      "refresh",
			"expires_in":         time.Now().Add(time.Hour).Unix(),
			"refresh_expires_in": time.Now().Add(2 * time.Hour).Unix(),
		})
	})
	authed := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if f.rejectNext.CompareAndSwap(true, false) || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("POST /invoice", authed(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &f.lastInvoice); err != nil {
			t.Errorf("invoice body: %v", err)
		}
		writeJSON(w, map[string]any{
			"invoice_id":    "inv-123",
			"qPay_shortUrl": "https://s.qpay.mn/abc",
			"qr_text":       "0002010102121531...",
			"qr_image":      "iVBORw0KGgo=",
			"urls": []map[string]string{
				{"name": "Khan bank", "description": "Хаан банк", "logo": "https://qpay.mn/q/logo/khanbank.png", "link": "khanbank://q?qPay_QRcode=0002"},
				{"name": "empty", "link": ""},
			},
		})
	}))
	mux.HandleFunc("POST /payment/check", authed(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, f.checkBody)
	}))
	mux.HandleFunc("DELETE /invoice/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		f.lastDeletion = "invoice:" + r.PathValue("id")
		writeJSON(w, map[string]any{})
	}))
	mux.HandleFunc("DELETE /payment/refund/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		f.lastDeletion = "refund:" + r.PathValue("id")
		writeJSON(w, map[string]any{})
	}))
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newTestAdapter(t *testing.T) (*Adapter, *fakeQPay) {
	t.Helper()
	fake := &fakeQPay{}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)
	a, err := New(Config{
		Username:    "user",
		Password:    "pass",
		Endpoint:    srv.URL,
		InvoiceCode: "TEST_INVOICE",
		CallbackURL: "https://merchant.example/qpay/callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, fake
}

func TestNewRequiresCredentials(t *testing.T) {
	_, err := New(Config{Username: "u"})
	if !errors.Is(err, core.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
	for _, field := range []string{"Password", "Endpoint", "InvoiceCode"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error %q does not name %s", err, field)
		}
	}
}

func TestCreateInvoiceMapsResponse(t *testing.T) {
	a, fake := newTestAdapter(t)
	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID:     "order-1",
		Amount:      15000,
		Description: "Coffee",
		CallbackURL: "https://merchant.example/override",
		Phone:       "99112233",
		ExpiresIn:   10 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inv.InvoiceID != "inv-123" || inv.OrderID != "order-1" || inv.Status != core.StatusPending {
		t.Errorf("invoice = %+v", inv)
	}
	if inv.QRText == "" || inv.QRImage != "iVBORw0KGgo=" || inv.PaymentURL != "https://s.qpay.mn/abc" {
		t.Errorf("qr/url not mapped: %+v", inv)
	}
	if len(inv.Deeplinks) != 1 || inv.Deeplinks[0].Link != "khanbank://q?qPay_QRcode=0002" || inv.Deeplinks[0].Name != "Khan bank" {
		t.Errorf("deeplinks = %+v", inv.Deeplinks)
	}
	if inv.ExpiresAt.IsZero() {
		t.Error("expires_at not set")
	}

	got := fake.lastInvoice
	want := map[string]any{
		"invoice_code":          "TEST_INVOICE",
		"sender_invoice_no":     "order-1",
		"invoice_receiver_code": "terminal",
		"invoice_description":   "Coffee",
		"amount":                float64(15000),
		"callback_url":          "https://merchant.example/override",
		"enable_expiry":         true,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("request %s = %v, want %v", k, got[k], v)
		}
	}
	if rd, _ := got["invoice_receiver_data"].(map[string]any); rd["phone"] != "99112233" {
		t.Errorf("receiver data = %v", got["invoice_receiver_data"])
	}
}

func TestCreateInvoiceRejectsFractionalAmount(t *testing.T) {
	a, _ := newTestAdapter(t)
	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o", Amount: 10.5})
	if !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestTokenIsReusedAndRenewedOnRejection(t *testing.T) {
	a, fake := newTestAdapter(t)
	fake.checkBody = `{"count":0,"paid_amount":0,"rows":[]}`
	ctx := context.Background()
	in := core.CheckInvoiceInput{InvoiceID: "inv-123"}

	for range 2 {
		if _, err := a.CheckInvoice(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	if n := fake.logins.Load(); n != 1 {
		t.Fatalf("logins after two calls = %d, want 1", n)
	}

	fake.rejectNext.Store(true)
	if _, err := a.CheckInvoice(ctx, in); err != nil {
		t.Fatalf("call after token rejection: %v", err)
	}
	if n := fake.logins.Load(); n != 2 {
		t.Fatalf("logins after rejection = %d, want 2", n)
	}
}

func TestCheckInvoicePaid(t *testing.T) {
	a, fake := newTestAdapter(t)
	fake.checkBody = `{"count":1,"paid_amount":15000,"rows":[{"payment_id":"pay-9","payment_status":"PAID","payment_amount":"15000.00","payment_currency":"MNT","payment_wallet":"khan","transaction_type":"P2P"}]}`
	st, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "inv-123", Amount: 15000})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != core.StatusPaid || st.PaidAmount != 15000 || st.TransactionID != "pay-9" || st.InvoiceID != "inv-123" {
		t.Errorf("status = %+v", st)
	}
}

func TestCheckInvoiceUnderpaidIsPending(t *testing.T) {
	a, fake := newTestAdapter(t)
	fake.checkBody = `{"count":1,"paid_amount":5000,"rows":[{"payment_id":"pay-9","payment_status":"PAID","payment_amount":5000}]}`
	st, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "inv-123", Amount: 15000})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != core.StatusPending || st.PaidAmount != 5000 {
		t.Errorf("status = %+v", st)
	}
}

func TestCheckInvoiceRequiresInvoiceID(t *testing.T) {
	a, _ := newTestAdapter(t)
	if _, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestCancelAndRefund(t *testing.T) {
	a, fake := newTestAdapter(t)
	ctx := context.Background()
	if err := a.CancelInvoice(ctx, core.CancelInvoiceInput{InvoiceID: "inv-123"}); err != nil {
		t.Fatal(err)
	}
	if fake.lastDeletion != "invoice:inv-123" {
		t.Errorf("cancel hit %q", fake.lastDeletion)
	}
	if err := a.Refund(ctx, core.RefundInput{InvoiceID: "inv-123", TransactionID: "pay-9"}); err != nil {
		t.Fatal(err)
	}
	if fake.lastDeletion != "refund:pay-9" {
		t.Errorf("refund hit %q", fake.lastDeletion)
	}
	if err := a.Refund(ctx, core.RefundInput{TransactionID: "pay-9", Amount: 100}); !errors.Is(err, core.ErrInvalidInput) {
		t.Errorf("partial refund err = %v, want ErrInvalidInput", err)
	}
}

func TestProviderErrorIsWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	a, err := New(Config{Username: "u", Password: "p", Endpoint: srv.URL, InvoiceCode: "c"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "x"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Provider != core.ProviderQPay || pe.Op != "check_invoice" {
		t.Fatalf("err = %v, want ProviderError for check_invoice", err)
	}
}

func TestSummarizePayments(t *testing.T) {
	row := func(status string) *qpay_v2.QpayRow {
		return &qpay_v2.QpayRow{PaymentID: "p", PaymentStatus: status, PaymentAmount: "100"}
	}
	tests := []struct {
		name string
		rows []*qpay_v2.QpayRow
		want core.Status
	}{
		{"no rows", nil, core.StatusPending},
		{"new", []*qpay_v2.QpayRow{row("NEW")}, core.StatusPending},
		{"unknown status never paid", []*qpay_v2.QpayRow{row("SOMETHING")}, core.StatusPending},
		{"paid wins", []*qpay_v2.QpayRow{row("FAILED"), row("PAID")}, core.StatusPaid},
		{"all failed", []*qpay_v2.QpayRow{row("FAILED"), row("FAILED")}, core.StatusFailed},
		{"failed then new", []*qpay_v2.QpayRow{row("FAILED"), row("NEW")}, core.StatusPending},
		{"refunded", []*qpay_v2.QpayRow{row("REFUNDED")}, core.StatusRefunded},
	}
	for _, tt := range tests {
		if got := summarizePayments(tt.rows).status; got != tt.want {
			t.Errorf("%s: status = %s, want %s", tt.name, got, tt.want)
		}
	}
}
