package hipay_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/techpartners-asia/payment-all-in-one/core"
	"github.com/techpartners-asia/payment-all-in-one/providers/hipay"
)

type fakeHiPay struct {
	checkoutStatus string

	mu   sync.Mutex
	sent map[string]map[string]any
}

func (f *fakeHiPay) body(path string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[path]
}

func (f *fakeHiPay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer secret-1" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.sent[r.URL.Path] = body
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/checkout":
		_, _ = w.Write([]byte(`{"code":1,"description":"SUCCESS","requestId":"req-1","checkoutId":"chk-1","expires":"2026-11-16 10:30:00","qrData":"0002010102HIPAY"}`))
	case r.Method == http.MethodGet && r.URL.Path == "/checkout/get/chk-1" && r.URL.Query().Get("entityId") == "entity-1":
		// HiPay wraps some responses in a one-element array.
		resp := map[string]any{"code": 1, "description": "SUCCESS", "amount": 1000, "status": f.checkoutStatus, "status_date": "2026-11-16 10:05:00"}
		if f.checkoutStatus == "paid" {
			resp["paymentId"] = "pay-1"
			resp["paymentType"] = "wallet"
		}
		_ = json.NewEncoder(w).Encode([]any{resp})
	case r.Method == http.MethodPost && r.URL.Path == "/checkout/cancel":
		_, _ = w.Write([]byte(`{"code":1,"description":"SUCCESS"}`))
	case r.Method == http.MethodPost && r.URL.Path == "/payment/cancel":
		_, _ = w.Write([]byte(`[{"code":1,"description":"SUCCESS","paymentId":"pay-1"}]`))
	default:
		_, _ = w.Write([]byte(`{"code":0,"description":"UNSUCCESS","message":"invalid or missing entityId parameter"}`))
	}
}

func newAdapter(t *testing.T, checkoutStatus string) (*fakeHiPay, *hipay.Adapter) {
	t.Helper()
	f := &fakeHiPay{checkoutStatus: checkoutStatus, sent: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	a, err := hipay.New(hipay.Config{Endpoint: srv.URL + "/", Token: "secret-1", EntityID: "entity-1"})
	if err != nil {
		t.Fatal(err)
	}
	return f, a
}

func TestNewRejectsMissingConfig(t *testing.T) {
	_, err := hipay.New(hipay.Config{Endpoint: "https://api.hipay.mn"})
	if !errors.Is(err, core.ErrInvalidConfig) || !strings.Contains(err.Error(), "Token") || !strings.Contains(err.Error(), "EntityID") {
		t.Fatalf("want ErrInvalidConfig naming Token and EntityID, got %v", err)
	}
}

func TestCreateInvoice(t *testing.T) {
	f, a := newAdapter(t, "new")
	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "order-1", Amount: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Provider != core.ProviderHiPay || inv.InvoiceID != "chk-1" || inv.OrderID != "order-1" ||
		inv.Amount != 1000 || inv.QRText != "0002010102HIPAY" || inv.Status != core.StatusPending || inv.Raw == nil {
		t.Fatalf("invoice = %+v", inv)
	}
	sent := f.body("/checkout")
	if sent["entityId"] != "entity-1" || sent["amount"] != float64(1000) || sent["qrData"] != true {
		t.Fatalf("request body = %v", sent)
	}
}

func TestCheckInvoiceStatusMapping(t *testing.T) {
	cases := []struct {
		provider string
		want     core.Status
		paid     float64
		txn      string
	}{
		{"paid", core.StatusPaid, 1000, "pay-1"},
		{"new", core.StatusPending, 0, ""},
		{"canceled", core.StatusCancelled, 0, ""},
		{"expired", core.StatusFailed, 0, ""},
		{"invalid", core.StatusFailed, 0, ""},
		{"something-new", core.StatusPending, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			_, a := newAdapter(t, tc.provider)
			st, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "chk-1"})
			if err != nil {
				t.Fatal(err)
			}
			if st.Status != tc.want || st.PaidAmount != tc.paid || st.TransactionID != tc.txn || st.InvoiceID != "chk-1" {
				t.Fatalf("status = %+v", st)
			}
		})
	}
}

func TestCancelInvoice(t *testing.T) {
	f, a := newAdapter(t, "new")
	if err := a.CancelInvoice(context.Background(), core.CancelInvoiceInput{InvoiceID: "chk-1"}); err != nil {
		t.Fatal(err)
	}
	if got := f.body("/checkout/cancel"); got["checkoutId"] != "chk-1" || got["entityId"] != "entity-1" {
		t.Fatalf("request body = %v", got)
	}
}

func TestRefund(t *testing.T) {
	f, a := newAdapter(t, "paid")
	ctx := context.Background()
	if err := a.Refund(ctx, core.RefundInput{TransactionID: "pay-1"}); err != nil {
		t.Fatal(err)
	}
	if got := f.body("/payment/cancel"); got["paymentId"] != "pay-1" || got["entityId"] != "entity-1" {
		t.Fatalf("request body = %v", got)
	}
	if err := a.Refund(ctx, core.RefundInput{TransactionID: "pay-1", Amount: 500}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("partial refund: want ErrUnsupported, got %v", err)
	}
	if err := a.Refund(ctx, core.RefundInput{InvoiceID: "chk-1"}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("missing TransactionID: want ErrInvalidInput, got %v", err)
	}
}

func TestMissingInvoiceIDIsInvalidInput(t *testing.T) {
	_, a := newAdapter(t, "new")
	ctx := context.Background()
	if _, err := a.CheckInvoice(ctx, core.CheckInvoiceInput{OrderID: "order-1"}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("check: want ErrInvalidInput, got %v", err)
	}
	if err := a.CancelInvoice(ctx, core.CancelInvoiceInput{}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("cancel: want ErrInvalidInput, got %v", err)
	}
}

func TestBusinessErrorIsWrapped(t *testing.T) {
	_, a := newAdapter(t, "new")
	_, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "unknown"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Provider != core.ProviderHiPay || pe.Op != "check_invoice" {
		t.Fatalf("want ProviderError{hipay, check_invoice}, got %v", err)
	}
}
