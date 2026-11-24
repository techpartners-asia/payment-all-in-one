package mongolchat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

type fakeMchat struct {
	lastGenerate map[string]any
	lastRefund   map[string]any
	statusBody   string
}

func newTestAdapter(t *testing.T) (*Adapter, *fakeMchat) {
	t.Helper()
	fake := &fakeMchat{}
	decode := func(r *http.Request, into *map[string]any) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, into); err != nil {
			t.Errorf("request body: %v", err)
		}
	}
	mux := http.NewServeMux()
	authed := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("api-key") != "key-1" || r.Header.Get("Authorization") != "WorkerKey worker-1" {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{"code":401,"message":"unauthorized"}`)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("POST /worker/onlineqr/generate", authed(func(w http.ResponseWriter, r *http.Request) {
		decode(r, &fake.lastGenerate)
		io.WriteString(w, `{"qr":"MCHAT-QR-0001","code":1000,"message":"success"}`)
	}))
	mux.HandleFunc("POST /worker/onlineqr/status", authed(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		decode(r, &req)
		if req["qr"] != "MCHAT-QR-0001" {
			t.Errorf("status qr = %v", req["qr"])
		}
		io.WriteString(w, fake.statusBody)
	}))
	mux.HandleFunc("POST /worker/transaction/refund", authed(func(w http.ResponseWriter, r *http.Request) {
		decode(r, &fake.lastRefund)
		io.WriteString(w, `{"code":1000,"message":"success"}`)
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a, err := New(Config{Endpoint: srv.URL + "/", APIKey: "key-1", WorkerKey: "worker-1", BranchID: "branch-7"})
	if err != nil {
		t.Fatal(err)
	}
	return a, fake
}

func TestNewRequiresCredentials(t *testing.T) {
	_, err := New(Config{Endpoint: "https://mchat.example"})
	if !errors.Is(err, core.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "APIKey") || !strings.Contains(err.Error(), "WorkerKey") {
		t.Errorf("error %q does not name missing fields", err)
	}
}

func TestCreateInvoice(t *testing.T) {
	a, fake := newTestAdapter(t)
	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID:     "order-1",
		Amount:      3000,
		Description: "Pizza",
		Extra:       map[string]string{"noat": "272.73", "sub_title": "Table 4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inv.InvoiceID != "MCHAT-QR-0001" || inv.QRText != "MCHAT-QR-0001" || inv.OrderID != "order-1" || inv.Status != core.StatusPending {
		t.Errorf("invoice = %+v", inv)
	}
	got := fake.lastGenerate
	want := map[string]any{
		"amount":           float64(3000),
		"branch_id":        "branch-7",
		"title":            "Pizza",
		"sub_title":        "Table 4",
		"noat":             "272.73",
		"reference_number": "order-1",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("request %s = %v, want %v", k, got[k], v)
		}
	}
	products, _ := got["products"].([]any)
	if len(products) != 1 {
		t.Fatalf("products = %v", got["products"])
	}
	if p := products[0].(map[string]any); p["product_name"] != "Pizza" || p["price"] != float64(3000) || p["quantity"] != "1" {
		t.Errorf("product = %v", p)
	}
}

func TestCreateInvoiceRejectsFractionalAmount(t *testing.T) {
	a, _ := newTestAdapter(t)
	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o", Amount: 99.5})
	if !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestCheckInvoice(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    core.Status
		wantTxn string
	}{
		{"paid", `{"status":"paid","code":1000,"message":"success","id":"txn-55","who_paid":"99112233","user_ref_id":"u-1"}`, core.StatusPaid, "txn-55"},
		{"pending", `{"status":"pending","code":1000,"message":"success"}`, core.StatusPending, ""},
		{"unknown status never paid", `{"status":"weird","code":1000,"message":"success","id":"txn-55"}`, core.StatusPending, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, fake := newTestAdapter(t)
			fake.statusBody = tt.body
			st, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "MCHAT-QR-0001"})
			if err != nil {
				t.Fatal(err)
			}
			if st.Status != tt.want || st.TransactionID != tt.wantTxn {
				t.Errorf("status = %+v", st)
			}
		})
	}
}

func TestCheckInvoiceBusinessError(t *testing.T) {
	a, fake := newTestAdapter(t)
	fake.statusBody = `{"code":2001,"message":"qr not found"}`
	_, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "MCHAT-QR-0001"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "check_invoice" || !strings.Contains(err.Error(), "qr not found") {
		t.Fatalf("err = %v, want wrapped check_invoice error", err)
	}
}

func TestTransportFailureReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint := srv.URL
	srv.Close()
	a, err := New(Config{Endpoint: endpoint, APIKey: "k", WorkerKey: "w"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "qr"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Provider != core.ProviderMongolChat {
		t.Fatalf("err = %v, want ProviderError", err)
	}
}

func TestRefund(t *testing.T) {
	a, fake := newTestAdapter(t)
	ctx := context.Background()
	if err := a.Refund(ctx, core.RefundInput{TransactionID: "txn-55", Amount: 3000, Reason: "customer request"}); err != nil {
		t.Fatal(err)
	}
	if fake.lastRefund["id"] != "txn-55" || fake.lastRefund["amount"] != float64(3000) || fake.lastRefund["reason"] != "customer request" {
		t.Errorf("refund request = %v", fake.lastRefund)
	}
	if err := a.Refund(ctx, core.RefundInput{TransactionID: "txn-55"}); !errors.Is(err, core.ErrInvalidInput) {
		t.Errorf("zero-amount refund err = %v, want ErrInvalidInput", err)
	}
}
