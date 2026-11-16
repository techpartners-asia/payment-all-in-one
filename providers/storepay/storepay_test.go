package storepay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

type fakeStorePay struct {
	mu          sync.Mutex
	authQuery   map[string]string
	loanBody    map[string]any
	checkedPath string
	confirmed   bool
	loanReply   any
}

func (f *fakeStorePay) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "client" || p != "secret" {
			http.Error(w, "bad client", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		f.mu.Lock()
		f.authQuery = map[string]string{"username": q.Get("username"), "password": q.Get("password"), "grant_type": q.Get("grant_type")}
		f.mu.Unlock()
		writeJSON(w, map[string]any{"access_token": "tok", "token_type": "bearer", "expires_in": 3600})
	})
	mux.HandleFunc("POST /merchant/loan", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode loan body: %v", err)
		}
		f.mu.Lock()
		f.loanBody = body
		reply := f.loanReply
		f.mu.Unlock()
		if reply == nil {
			reply = map[string]any{"value": 98765, "msgList": []any{}, "status": "Success"}
		}
		writeJSON(w, reply)
	})
	mux.HandleFunc("GET /merchant/loan/check/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.checkedPath = r.URL.EscapedPath()
		confirmed := f.confirmed
		f.mu.Unlock()
		writeJSON(w, map[string]any{"value": confirmed, "msgList": []any{}, "status": "Success"})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func testConfig(url string) Config {
	return Config{
		AppUsername: "app user",
		AppPassword: "p&ss=word",
		Username:    "client",
		Password:    "secret",
		AuthURL:     url,
		BaseURL:     url,
		StoreID:     "store-7",
		CallbackURL: "https://merchant.test/cb",
	}
}

func newTestAdapter(t *testing.T, f *fakeStorePay) *Adapter {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	a, err := New(testConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNewRequiresConfig(t *testing.T) {
	cfg := testConfig("https://storepay.test")
	cfg.StoreID = ""
	_, err := New(cfg)
	if !errors.Is(err, core.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestCreateInvoiceRequestsLoan(t *testing.T) {
	f := &fakeStorePay{}
	a := newTestAdapter(t, f)

	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID: "order-1",
		Amount:  250000,
		Phone:   "99112233",
	})
	if err != nil {
		t.Fatal(err)
	}

	if inv.InvoiceID != "98765" || inv.OrderID != "order-1" || inv.Status != core.StatusPending || inv.Raw != int64(98765) {
		t.Errorf("invoice = %+v", inv)
	}
	want := map[string]any{
		"storeId":      "store-7",
		"mobileNumber": "99112233",
		"description":  "order-1",
		"amount":       "250000.000000",
		"callbackUrl":  "https://merchant.test/cb",
	}
	for k, v := range want {
		if f.loanBody[k] != v {
			t.Errorf("loan body %s = %v, want %v", k, f.loanBody[k], v)
		}
	}
	if f.authQuery["username"] != "app user" || f.authQuery["password"] != "p&ss=word" || f.authQuery["grant_type"] != "password" {
		t.Errorf("token request query = %v", f.authQuery)
	}
}

func TestCreateInvoiceCallbackOverride(t *testing.T) {
	f := &fakeStorePay{}
	a := newTestAdapter(t, f)

	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		Amount:      1000,
		Phone:       "99112233",
		Description: "phone case",
		CallbackURL: "https://merchant.test/override",
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.loanBody["callbackUrl"] != "https://merchant.test/override" || f.loanBody["description"] != "phone case" {
		t.Errorf("loan body = %v", f.loanBody)
	}
}

func TestCreateInvoiceRequiresPhone(t *testing.T) {
	a := newTestAdapter(t, &fakeStorePay{})
	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o", Amount: 1000})
	if !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestCreateInvoiceProviderRejection(t *testing.T) {
	cases := map[string]any{
		"with message": map[string]any{"status": "Failed", "msgList": []any{map[string]any{"code": "LIMIT", "text": "insufficient limit"}}},
		// The SDK indexes msgList[0] unconditionally on failure.
		"empty msgList": map[string]any{"status": "Failed", "msgList": []any{}},
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeStorePay{loanReply: reply}
			a := newTestAdapter(t, f)

			_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{Amount: 1000, Phone: "99112233"})
			var pe *core.ProviderError
			if !errors.As(err, &pe) || pe.Provider != core.ProviderStorePay || pe.Op != "create_invoice" {
				t.Fatalf("err = %v, want ProviderError for storepay create_invoice", err)
			}
		})
	}
}

func TestCheckInvoiceMapsLoanConfirmation(t *testing.T) {
	for _, tc := range []struct {
		confirmed bool
		want      core.Status
	}{
		{true, core.StatusPaid},
		{false, core.StatusPending},
	} {
		f := &fakeStorePay{confirmed: tc.confirmed}
		a := newTestAdapter(t, f)

		got, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "98765"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != tc.want || got.InvoiceID != "98765" {
			t.Errorf("confirmed=%v: status = %+v", tc.confirmed, got)
		}
		if f.checkedPath != "/merchant/loan/check/98765" {
			t.Errorf("checked path = %q", f.checkedPath)
		}
	}
}

func TestCheckInvoiceRequiresInvoiceID(t *testing.T) {
	a := newTestAdapter(t, &fakeStorePay{})
	_, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{})
	if !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestTransportFailureAfterAuthReturnsError(t *testing.T) {
	f := &fakeStorePay{}
	srv := httptest.NewServer(f.handler(t))
	a, err := New(testConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "1"}); err != nil {
		t.Fatal(err)
	}
	srv.Close()

	_, err = a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "1"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want ProviderError", err)
	}
}
