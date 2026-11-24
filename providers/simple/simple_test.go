package simple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

type fakeSimple struct {
	mu            sync.Mutex
	tokens        int
	created       map[string]any
	assignedQuery string
	checkedOrder  string
	invoiceStatus string
	createReply   map[string]any
}

func (f *fakeSimple) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mbank-auth-main-service/token", func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "user" || p != "pass" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.tokens++
		f.mu.Unlock()
		writeJSON(w, map[string]any{"code": "200", "data": map[string]any{"access_token": "tok", "expires_in": 3600}})
	})
	mux.HandleFunc("POST /mbank-integration-gateway-service/integration/addInvoice/createInvoice", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode create body: %v", err)
		}
		f.mu.Lock()
		f.created = body
		reply := f.createReply
		f.mu.Unlock()
		if reply == nil {
			reply = map[string]any{"code": "200", "message": "success", "data": "inv-uuid-1"}
		}
		writeJSON(w, reply)
	})
	mux.HandleFunc("POST /mbank-integration-gateway-service/integration/addInvoice/assignInvoiceMobile", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.assignedQuery = r.URL.RawQuery
		f.mu.Unlock()
		writeJSON(w, map[string]any{"code": "200", "data": map[string]any{"invoice_uuid": "inv-uuid-1"}})
	})
	mux.HandleFunc("GET /mbank-integration-gateway-service/integration/checkInvoice/merchant", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.checkedOrder = r.URL.Query().Get("order_id")
		status := f.invoiceStatus
		f.mu.Unlock()
		writeJSON(w, map[string]any{"code": "200", "data": map[string]any{
			"invoice_uuid":   "inv-uuid-1",
			"order_id":       "order 1&x",
			"total":          15000,
			"invoice_status": status,
			"simple_id":      "S-77",
		}})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newTestAdapter(t *testing.T, f *fakeSimple) *Adapter {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	a, err := New(Config{Username: "user", Password: "pass", BaseURL: srv.URL, CallbackURL: "https://merchant.test/cb"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNewRequiresCredentials(t *testing.T) {
	_, err := New(Config{BaseURL: "https://simple.test"})
	if !errors.Is(err, core.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestCreateInvoiceAssignsToPhoneAndOverridesCallback(t *testing.T) {
	f := &fakeSimple{}
	a := newTestAdapter(t, f)

	before := time.Now()
	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID:     "order-1",
		Amount:      15000,
		Phone:       "99112233",
		CallbackURL: "https://merchant.test/override",
		ExpiresIn:   5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	if inv.InvoiceID != "order-1" || inv.Status != core.StatusPending || inv.Amount != 15000 {
		t.Errorf("invoice = %+v", inv)
	}
	if d := inv.ExpiresAt.Sub(before); d < 5*time.Minute || d > 5*time.Minute+5*time.Second {
		t.Errorf("ExpiresAt is %v after create, want about 5m", d)
	}
	if f.created["callback_url"] != "https://merchant.test/override" {
		t.Errorf("callback_url = %v", f.created["callback_url"])
	}
	if f.created["total"] != float64(15000) || f.created["order_id"] != "order-1" {
		t.Errorf("create body = %v", f.created)
	}
	wantExpire := inv.ExpiresAt.In(time.FixedZone("", 8*3600)).Format(time.DateTime)
	if f.created["expire_date"] != wantExpire {
		t.Errorf("expire_date = %v, want %v (UTC+8)", f.created["expire_date"], wantExpire)
	}
	if f.assignedQuery != "invoice_uuid=inv-uuid-1&mobile=99112233" {
		t.Errorf("assign query = %q", f.assignedQuery)
	}
}

func TestTokenIsReusedPerCallbackURL(t *testing.T) {
	f := &fakeSimple{}
	a := newTestAdapter(t, f)
	ctx := context.Background()

	for i, cb := range []string{"https://merchant.test/override", "https://merchant.test/override", "", "https://merchant.test/cb"} {
		in := core.CreateInvoiceInput{OrderID: fmt.Sprintf("order-%d", i), Amount: 1000, CallbackURL: cb}
		if _, err := a.CreateInvoice(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.CheckInvoice(ctx, core.CheckInvoiceInput{InvoiceID: "order-0"}); err != nil {
		t.Fatal(err)
	}
	if f.tokens != 2 {
		t.Fatalf("token requests = %d, want 2 (one per distinct callback URL)", f.tokens)
	}
}

func TestCreateInvoiceDefaultsExpiryAndSkipsAssignWithoutPhone(t *testing.T) {
	f := &fakeSimple{}
	a := newTestAdapter(t, f)

	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "order-2", Amount: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(inv.ExpiresAt); d < 19*time.Minute || d > 20*time.Minute {
		t.Errorf("ExpiresAt in %v, want about 20m", d)
	}
	if f.created["callback_url"] != "https://merchant.test/cb" {
		t.Errorf("callback_url = %v", f.created["callback_url"])
	}
	if f.assignedQuery != "" {
		t.Errorf("assign called without phone: %q", f.assignedQuery)
	}
}

func TestCreateInvoiceRejectsFractionalAmount(t *testing.T) {
	a := newTestAdapter(t, &fakeSimple{})
	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o", Amount: 10.5})
	if !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestCheckInvoiceMapsStatus(t *testing.T) {
	cases := []struct {
		providerStatus string
		want           core.Status
		wantPaid       float64
	}{
		{"PAID", core.StatusPaid, 15000},
		{"COMPLETED", core.StatusPaid, 15000},
		{"NEW", core.StatusPending, 0},
		{"", core.StatusPending, 0},
	}
	for _, tc := range cases {
		t.Run(tc.providerStatus, func(t *testing.T) {
			f := &fakeSimple{invoiceStatus: tc.providerStatus}
			a := newTestAdapter(t, f)

			got, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "order 1&x"})
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.want || got.PaidAmount != tc.wantPaid || got.InvoiceID != "order 1&x" {
				t.Errorf("status = %+v", got)
			}
			if f.checkedOrder != "order 1&x" {
				t.Errorf("provider saw order_id %q", f.checkedOrder)
			}
		})
	}
}

func TestCheckInvoiceRequiresInvoiceID(t *testing.T) {
	a := newTestAdapter(t, &fakeSimple{})
	_, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{})
	if !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestProviderErrorIsWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	a, err := New(Config{Username: "u", Password: "p", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	_, err = a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "o"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Provider != core.ProviderSimple || pe.Op != "check_invoice" {
		t.Fatalf("err = %v, want ProviderError for simple check_invoice", err)
	}
}

func TestTransportFailureAfterAuthReturnsError(t *testing.T) {
	f := &fakeSimple{}
	srv := httptest.NewServer(f.handler(t))
	a, err := New(Config{Username: "user", Password: "pass", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "o"}); err != nil {
		t.Fatal(err)
	}
	srv.Close()

	// The token is cached, so the SDK goes straight to a request that fails in
	// transport, where it dereferences a nil response.
	_, err = a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "o"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want ProviderError", err)
	}
}

func TestCreateInvoiceErrorCodeWithHTTP200IsAnError(t *testing.T) {
	f := &fakeSimple{createReply: map[string]any{"code": "ORDER_DUPLICATE", "message": "duplicate order", "data": ""}}
	a := newTestAdapter(t, f)

	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "order-3", Amount: 1000, Phone: "99112233"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "create_invoice" {
		t.Fatalf("err = %v, want create_invoice ProviderError", err)
	}
	if f.assignedQuery != "" {
		t.Errorf("assigned an invoice that was never created: %q", f.assignedQuery)
	}
}
