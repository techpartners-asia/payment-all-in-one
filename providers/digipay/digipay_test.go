package digipay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

func testConfig(baseURL string) Config {
	return Config{
		BaseURL:      baseURL,
		ClientID:     "cid",
		ClientSecret: "csecret",
		MerchantID:   "m1",
		MerchantName: "Shop",
		TerminalID:   "t1",
		CallbackURL:  "https://shop.test/hook",
	}
}

func TestNewRequiresCredentials(t *testing.T) {
	_, err := New(Config{BaseURL: "https://x"})
	if !errors.Is(err, core.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
	if _, err := New(testConfig("https://x")); err != nil {
		t.Fatalf("valid config: %v", err)
	}
}

func TestCreateInvoice(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v3/superapp/save/invoice" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"invoiceId":"inv-1","redirectUrl":"https://digipay.test/pay/inv-1"}`))
	}))
	defer srv.Close()

	a, err := New(testConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID:     "order-1",
		Amount:      1500,
		CallbackURL: "https://shop.test/override",
		Extra:       map[string]string{"bearer_token": "user-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inv.InvoiceID != "inv-1" || inv.PaymentURL != "https://digipay.test/pay/inv-1" ||
		inv.OrderID != "order-1" || inv.Amount != 1500 || inv.Status != core.StatusPending {
		t.Fatalf("invoice = %+v", inv)
	}
	if gotAuth != "Bearer user-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotBody["webHookUrl"] != "https://shop.test/override" || gotBody["merchantId"] != "m1" {
		t.Fatalf("body = %v", gotBody)
	}
}

func TestCreateInvoiceRequiresBearerToken(t *testing.T) {
	a, _ := New(testConfig("https://x"))
	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{Amount: 100})
	if !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestCheckInvoice(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		wantStatus core.Status
		wantAmount float64
	}{
		{"paid", "paid", core.StatusPaid, 1500},
		{"success", "SUCCESS", core.StatusPaid, 1500},
		{"unknown", "pending", core.StatusPending, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/wallet/auth/token", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"access_token":"client-token"}`))
			})
			mux.HandleFunc("GET /v3/superapp/check/invoice", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("invoiceId") != "inv-1" || r.Header.Get("Authorization") != "Bearer client-token" {
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"amount": 1500, "merchantId": "m1", "invoiceId": "inv-1", "txnId": "txn-9", "status": tt.status,
				})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			a, _ := New(testConfig(srv.URL))
			ps, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "inv-1"})
			if err != nil {
				t.Fatal(err)
			}
			if ps.Status != tt.wantStatus || ps.PaidAmount != tt.wantAmount || ps.TransactionID != "txn-9" || ps.InvoiceID != "inv-1" {
				t.Fatalf("status = %+v", ps)
			}
		})
	}
}

func TestCheckInvoiceWrapsProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	a, _ := New(testConfig(srv.URL))
	_, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "inv-1"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "check_invoice" {
		t.Fatalf("err = %v, want ProviderError check_invoice", err)
	}
}

func TestCheckInvoiceRequiresInvoiceID(t *testing.T) {
	a, _ := New(testConfig("https://x"))
	_, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{})
	if !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestCreateInvoiceWithoutInvoiceIDIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	a, err := New(testConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID: "order-1", Amount: 1500, Extra: map[string]string{"bearer_token": "user-token"},
	})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "create_invoice" {
		t.Fatalf("err = %v, want create_invoice ProviderError", err)
	}
}
