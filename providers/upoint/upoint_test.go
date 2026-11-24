package upoint

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

// newTestAdapter serves each UPoint path from responses and records the
// decoded request body per path.
func newTestAdapter(t *testing.T, responses map[string]string) (*Adapter, map[string]map[string]any) {
	t.Helper()
	got := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := responses[r.URL.Path]
		if !ok || r.Header.Get("Authorization") != "Token tok" {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		got[r.URL.Path] = req
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	a, err := New(Config{Endpoint: srv.URL, Token: "tok", TerminalID: "term-1"})
	if err != nil {
		t.Fatal(err)
	}
	return a, got
}

func TestNewRequiresCredentials(t *testing.T) {
	_, err := New(Config{Endpoint: "https://x"})
	if !errors.Is(err, core.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestCreateInvoiceSpendsPoints(t *testing.T) {
	const path = "/transaction/thirdparty/process_transaction/"
	a, got := newTestAdapter(t, map[string]string{
		path: `{"receipt_id":"R-1","date":"2026/11/16 10:00:00","card_number":"9496","point_balance":4000,"total_point":0,"spend_point":1000,"bill_number":"order-1","result":0,"message":"success"}`,
	})
	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID: "order-1",
		Amount:  1000,
		Phone:   "99112233",
		Extra:   map[string]string{"card_number": "9496", "total_amount": "5000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inv.InvoiceID != "order-1" || inv.Status != core.StatusPaid || inv.Amount != 1000 {
		t.Fatalf("invoice = %+v", inv)
	}
	req := got[path]
	if req["card_number"] != "9496" || req["mobile"] != "99112233" || req["bill_number"] != "order-1" ||
		req["spend_amount"] != 1000.0 || req["total_amount"] != 5000.0 || req["bonus_amount"] != 0.0 ||
		req["terminal_id"] != "term-1" {
		t.Fatalf("request = %v", req)
	}
	if _, err := time.Parse(billDateLayout, req["date"].(string)); err != nil {
		t.Fatalf("date %q: %v", req["date"], err)
	}
	if items, ok := req["items"].([]any); !ok || len(items) != 0 {
		t.Fatalf("items = %#v, want empty array", req["items"])
	}
}

func TestCreateInvoiceBusinessError(t *testing.T) {
	a, _ := newTestAdapter(t, map[string]string{
		"/transaction/thirdparty/process_transaction/": `{"result":5,"message":"insufficient balance"}`,
	})
	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID: "order-1", Amount: 1000, Extra: map[string]string{"card_number": "9496"},
	})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "create_invoice" {
		t.Fatalf("err = %v, want ProviderError create_invoice", err)
	}
}

func TestCreateInvoiceTransportFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	a, _ := New(Config{Endpoint: srv.URL, Token: "tok", TerminalID: "term-1"})
	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID: "order-1", Amount: 1000, Extra: map[string]string{"card_number": "9496"},
	})
	var pe *core.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want ProviderError", err)
	}
}

func TestCheckInvoice(t *testing.T) {
	const path = "/transaction/thirdparty/check_transaction/"
	tests := []struct {
		name       string
		body       string
		wantStatus core.Status
		wantAmount float64
		wantTxn    string
	}{
		{"found", `{"receipt_id":"R-1","bill_number":"order-1","spend_point":1000,"point_balance":4000,"result":0,"message":"success"}`, core.StatusPaid, 1000, "R-1"},
		{"not found", `{"result":1,"message":"transaction not found"}`, core.StatusPending, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, got := newTestAdapter(t, map[string]string{path: tt.body})
			ps, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "order-1"})
			if err != nil {
				t.Fatal(err)
			}
			if ps.Status != tt.wantStatus || ps.PaidAmount != tt.wantAmount || ps.TransactionID != tt.wantTxn {
				t.Fatalf("status = %+v", ps)
			}
			if got[path]["bill_number"] != "order-1" || got[path]["terminal_id"] != "term-1" {
				t.Fatalf("request = %v", got[path])
			}
		})
	}
}

func TestRefundFullCancelsByBillNumber(t *testing.T) {
	const path = "/transaction/thirdparty/cancel_transaction/"
	a, got := newTestAdapter(t, map[string]string{
		path: `{"receipt_id":"R-1","return_receipt_id":9,"refund_spend_point":1000,"result":0,"message":"success","bill_number":"order-1"}`,
	})
	if err := a.Refund(context.Background(), core.RefundInput{InvoiceID: "order-1"}); err != nil {
		t.Fatal(err)
	}
	if got[path]["bill_number"] != "order-1" {
		t.Fatalf("request = %v", got[path])
	}
}

func TestRefundPartialReturnsByReceipt(t *testing.T) {
	const path = "/transaction/thirdparty/return_transaction/"
	a, got := newTestAdapter(t, map[string]string{
		path: `{"receipt_id":"R-1","return_receipt_id":9,"refund_spend_point":400,"result":0,"message":"success"}`,
	})
	if err := a.Refund(context.Background(), core.RefundInput{TransactionID: "R-1", Amount: 400}); err != nil {
		t.Fatal(err)
	}
	if got[path]["receipt_id"] != "R-1" || got[path]["refund_spend_amount"] != 400.0 || got[path]["terminal_id"] != "term-1" {
		t.Fatalf("request = %v", got[path])
	}
}

func TestRefundBusinessError(t *testing.T) {
	a, _ := newTestAdapter(t, map[string]string{
		"/transaction/thirdparty/return_transaction/": `{"result":3,"message":"receipt not found"}`,
	})
	err := a.Refund(context.Background(), core.RefundInput{TransactionID: "R-x", Amount: 10})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "refund" {
		t.Fatalf("err = %v, want ProviderError refund", err)
	}
}

func TestInvalidInput(t *testing.T) {
	a, _ := New(Config{Endpoint: "https://x", Token: "t", TerminalID: "term"})
	ctx := context.Background()
	cases := map[string]error{
		"create without card or phone": func() error {
			_, err := a.CreateInvoice(ctx, core.CreateInvoiceInput{OrderID: "o", Amount: 1})
			return err
		}(),
		"create without order": func() error {
			_, err := a.CreateInvoice(ctx, core.CreateInvoiceInput{Amount: 1, Phone: "9911"})
			return err
		}(),
		"create with bad total": func() error {
			_, err := a.CreateInvoice(ctx, core.CreateInvoiceInput{OrderID: "o", Amount: 1, Phone: "9911", Extra: map[string]string{"total_amount": "abc"}})
			return err
		}(),
		"check without id": func() error {
			_, err := a.CheckInvoice(ctx, core.CheckInvoiceInput{})
			return err
		}(),
		"full refund without bill":       a.Refund(ctx, core.RefundInput{}),
		"partial refund without receipt": a.Refund(ctx, core.RefundInput{InvoiceID: "o", Amount: 5}),
	}
	for name, err := range cases {
		if !errors.Is(err, core.ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
}

func TestCheckInvoiceHTTPErrorIsReadable(t *testing.T) {
	a, _ := newTestAdapter(t, map[string]string{})
	_, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "order-1"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Err.Error() != "non-200 HTTP response" {
		t.Fatalf("err = %v, want ProviderError with non-200 message", err)
	}
}

func TestUndecodableSuccessIsNotPaid(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         ``,
		"string result": `{"result":"115","message":"card not found"}`,
		"auth error":    `{"detail":"Invalid token."}`,
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := newTestAdapter(t, map[string]string{
				"/transaction/thirdparty/process_transaction/": body,
				"/transaction/thirdparty/cancel_transaction/":  body,
				"/transaction/thirdparty/return_transaction/":  body,
			})
			inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
				OrderID: "order-1", Amount: 1000, Extra: map[string]string{"card_number": "9496"},
			})
			if err == nil {
				t.Fatalf("create returned %+v, want error", inv)
			}
			if err := a.Refund(context.Background(), core.RefundInput{OrderID: "order-1"}); err == nil {
				t.Fatal("full refund returned nil, want error")
			}
			if err := a.Refund(context.Background(), core.RefundInput{TransactionID: "r1", Amount: 10}); err == nil {
				t.Fatal("partial refund returned nil, want error")
			}
		})
	}
}
