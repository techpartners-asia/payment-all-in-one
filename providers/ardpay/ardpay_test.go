package ardpay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

func newTestAdapter(t *testing.T, handler http.HandlerFunc) *Adapter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	a, err := New(Config{BaseURL: srv.URL, MerchantID: "payee-1", PosNo: "pos-1", APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNewRequiresCredentials(t *testing.T) {
	_, err := New(Config{BaseURL: "https://x", MerchantID: "m"})
	if !errors.Is(err, core.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestCreateInvoice(t *testing.T) {
	var got map[string]any
	var gotKey string
	a := newTestAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/resources/merch/v1.0/createqr" {
			http.NotFound(w, r)
			return
		}
		gotKey = r.Header.Get("APIKEY")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"responseCode":0,"responseDesc":"OK","qrCode":"QR-123","qrLink":"https://ard.test/qr/QR-123","qpayAccountId":"acc"}`))
	})

	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID:     "order-1",
		Amount:      2500,
		Description: "Order 1",
		Extra:       map[string]string{"paid_limit": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inv.InvoiceID != "QR-123" || inv.QRText != "QR-123" || inv.PaymentURL != "https://ard.test/qr/QR-123" ||
		inv.OrderID != "order-1" || inv.Status != core.StatusPending {
		t.Fatalf("invoice = %+v", inv)
	}
	if gotKey != "key" {
		t.Fatalf("APIKEY = %q", gotKey)
	}
	if got["invoiceId"] != "order-1" || got["payeeCode"] != "payee-1" || got["tranCur"] != "MNT" ||
		got["tranAmnt"] != 2500.0 || got["paidLimit"] != 1.0 {
		t.Fatalf("request = %v", got)
	}
}

func TestCreateInvoiceProviderError(t *testing.T) {
	a := newTestAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"responseCode":12,"responseDesc":"invalid pos"}`))
	})
	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o", Amount: 10})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "create_invoice" {
		t.Fatalf("err = %v, want ProviderError create_invoice", err)
	}
}

func TestCheckInvoice(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus core.Status
		wantAmount float64
		wantTxn    string
	}{
		{
			name:       "paid",
			body:       `{"responseCode":0,"qrList":[{"qrCode":"QR-123","paymentId":77,"tranId":"T-1","tranDate":"2026-11-16","tranAmnt":2500,"invoiceId":"order-1","responseCode":0,"responseDesc":"paid"}]}`,
			wantStatus: core.StatusPaid,
			wantAmount: 2500,
			wantTxn:    "T-1",
		},
		{
			name:       "not paid",
			body:       `{"responseCode":0,"qrList":[{"qrCode":"QR-123","responseCode":1,"responseDesc":"not paid"}]}`,
			wantStatus: core.StatusPending,
		},
		{
			name:       "missing from list",
			body:       `{"responseCode":0,"qrList":[]}`,
			wantStatus: core.StatusPending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestAdapter(t, func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					QrCode []string `json:"qrCode"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				if r.URL.Path != "/resources/merch/v1.0/checkqr" || len(req.QrCode) != 1 || req.QrCode[0] != "QR-123" {
					http.Error(w, "bad", http.StatusBadRequest)
					return
				}
				_, _ = w.Write([]byte(tt.body))
			})
			ps, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "QR-123"})
			if err != nil {
				t.Fatal(err)
			}
			if ps.Status != tt.wantStatus || ps.PaidAmount != tt.wantAmount || ps.TransactionID != tt.wantTxn {
				t.Fatalf("status = %+v", ps)
			}
		})
	}
}

func TestCancelInvoice(t *testing.T) {
	a := newTestAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/resources/merch/v1.0/cancelqr" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"responseCode":0,"qrlist":[{"qrCode":"QR-123","responseCode":0}]}`))
	})
	if err := a.CancelInvoice(context.Background(), core.CancelInvoiceInput{InvoiceID: "QR-123"}); err != nil {
		t.Fatal(err)
	}
}

func TestCancelInvoicePerQrFailure(t *testing.T) {
	a := newTestAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"responseCode":0,"qrlist":[{"qrCode":"QR-123","responseCode":5,"responseDesc":"already paid"}]}`))
	})
	err := a.CancelInvoice(context.Background(), core.CancelInvoiceInput{InvoiceID: "QR-123"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "cancel_invoice" {
		t.Fatalf("err = %v, want ProviderError cancel_invoice", err)
	}
}

func TestInvalidInput(t *testing.T) {
	a, _ := New(Config{BaseURL: "https://x", MerchantID: "m", PosNo: "p", APIKey: "k"})
	ctx := context.Background()
	if _, err := a.CreateInvoice(ctx, core.CreateInvoiceInput{Amount: 10}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("create without OrderID: %v", err)
	}
	if _, err := a.CreateInvoice(ctx, core.CreateInvoiceInput{OrderID: "o", Amount: 10, Extra: map[string]string{"paid_limit": "x"}}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("create with bad paid_limit: %v", err)
	}
	if _, err := a.CheckInvoice(ctx, core.CheckInvoiceInput{}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("check without InvoiceID: %v", err)
	}
	if err := a.CancelInvoice(ctx, core.CancelInvoiceInput{}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("cancel without InvoiceID: %v", err)
	}
}

func TestCreateInvoiceRejectsUndecodedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(`{"responseCode":0,"qrCode":"QR-123"}`))
	}))
	defer srv.Close()
	a, _ := New(Config{BaseURL: srv.URL, MerchantID: "m", PosNo: "p", APIKey: "k"})
	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o", Amount: 10})
	var pe *core.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want ProviderError", err)
	}
}

func TestCancelInvoiceMissingQrIsAnError(t *testing.T) {
	a := newTestAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"responseCode":0,"qrlist":[]}`))
	})
	err := a.CancelInvoice(context.Background(), core.CancelInvoiceInput{InvoiceID: "QR-123"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "cancel_invoice" {
		t.Fatalf("err = %v, want ProviderError cancel_invoice", err)
	}
}
