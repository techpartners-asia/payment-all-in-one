package tino_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/techpartners-asia/payment-all-in-one/core"
	"github.com/techpartners-asia/payment-all-in-one/providers/tino"
)

type fakeTino struct {
	logins       atomic.Int32
	rejectNext   atomic.Bool
	checkStatus  string
	lastCreate   map[string]any
	lastCreateMu sync.Mutex
}

func (f *fakeTino) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/auth/merchant/login" {
			n := f.logins.Add(1)
			writeJSON(w, map[string]any{"status": true, "data": map[string]any{
				"token":      "tok-" + string(rune('0'+n)),
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			}})
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-") {
			t.Errorf("request %s without bearer token", r.URL.Path)
		}
		if f.rejectNext.CompareAndSwap(true, false) {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]any{"status": false, "message": "token revoked"})
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/pay/merchant/invoice":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.lastCreateMu.Lock()
			f.lastCreate = body
			f.lastCreateMu.Unlock()
			writeJSON(w, map[string]any{"status": true, "data": map[string]any{
				"invoice_id": "INV-1",
				"amount":     15000,
				"status":     "created",
				"qr_code":    "0002010102QR",
				"deep_link":  "tino://pay?invoice=INV-1",
				"expires_at": "2026-11-16T10:30:00Z",
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/pay/merchant/invoice/INV-1":
			writeJSON(w, map[string]any{"status": true, "data": map[string]any{
				"invoice_id": "INV-1",
				"status":     f.checkStatus,
				"amount":     14000,
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/pay/merchant/invoice/cancel/INV-1":
			writeJSON(w, map[string]any{"status": true, "data": map[string]any{"status": "cancelled"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

func newAdapter(t *testing.T, f *fakeTino) *tino.Adapter {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	a, err := tino.New(tino.Config{
		AuthURL:     srv.URL + "/auth",
		BaseURL:     srv.URL + "/pay",
		Username:    "merchant",
		Password:    "secret",
		WalletID:    "wallet-1",
		CallbackURL: "https://shop.example/default-callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNewRejectsMissingConfig(t *testing.T) {
	_, err := tino.New(tino.Config{AuthURL: "https://auth", BaseURL: "https://pay"})
	if !errors.Is(err, core.ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
	for _, field := range []string{"Username", "Password", "WalletID"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error %q does not name %s", err, field)
		}
	}
}

func TestCreateInvoiceMapsResponseAndReusesToken(t *testing.T) {
	f := &fakeTino{checkStatus: "pending"}
	a := newAdapter(t, f)
	ctx := context.Background()

	inv, err := a.CreateInvoice(ctx, core.CreateInvoiceInput{
		OrderID:     "order-1",
		Amount:      15000,
		Description: "Order #1",
		CallbackURL: "https://shop.example/override",
		ExpiresIn:   90 * time.Second,
		Extra:       map[string]string{"wallet_id": "wallet-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Provider != core.ProviderTino || inv.InvoiceID != "INV-1" || inv.OrderID != "order-1" ||
		inv.Amount != 15000 || inv.QRText != "0002010102QR" || inv.Status != core.StatusPending ||
		!inv.ExpiresAt.Equal(time.Date(2026, 11, 16, 10, 30, 0, 0, time.UTC)) || inv.Raw == nil ||
		len(inv.Deeplinks) != 1 || inv.Deeplinks[0] != (core.Deeplink{Name: "Tino", Link: "tino://pay?invoice=INV-1"}) {
		t.Fatalf("invoice = %+v", inv)
	}

	f.lastCreateMu.Lock()
	sent := f.lastCreate
	f.lastCreateMu.Unlock()
	if sent["callback_url"] != "https://shop.example/override" || sent["wallet_id"] != "wallet-2" ||
		sent["transaction_id"] != "order-1" || sent["expires_minute"] != float64(2) {
		t.Fatalf("request body = %v", sent)
	}

	if _, err := a.CheckInvoice(ctx, core.CheckInvoiceInput{InvoiceID: "INV-1"}); err != nil {
		t.Fatal(err)
	}
	if n := f.logins.Load(); n != 1 {
		t.Fatalf("logins = %d, want 1 for two calls within the token lifetime", n)
	}
}

func TestCheckInvoiceStatusMapping(t *testing.T) {
	cases := []struct {
		provider string
		want     core.Status
		paid     float64
	}{
		{"paid", core.StatusPaid, 14000},
		{"PAID", core.StatusPaid, 14000},
		{"pending", core.StatusPending, 0},
		{"expired", core.StatusFailed, 0},
		{"cancelled", core.StatusCancelled, 0},
		{"something-new", core.StatusPending, 0},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			a := newAdapter(t, &fakeTino{checkStatus: tc.provider})
			st, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "INV-1"})
			if err != nil {
				t.Fatal(err)
			}
			if st.Status != tc.want || st.PaidAmount != tc.paid || st.InvoiceID != "INV-1" {
				t.Fatalf("status = %+v, want %s paid %v", st, tc.want, tc.paid)
			}
		})
	}
}

func TestRejectedTokenTriggersOneReloginAndRetry(t *testing.T) {
	f := &fakeTino{checkStatus: "paid"}
	a := newAdapter(t, f)
	ctx := context.Background()
	if _, err := a.CheckInvoice(ctx, core.CheckInvoiceInput{InvoiceID: "INV-1"}); err != nil {
		t.Fatal(err)
	}

	f.rejectNext.Store(true)
	st, err := a.CheckInvoice(ctx, core.CheckInvoiceInput{InvoiceID: "INV-1"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != core.StatusPaid {
		t.Fatalf("status = %s", st.Status)
	}
	if n := f.logins.Load(); n != 2 {
		t.Fatalf("logins = %d, want 2", n)
	}
}

func TestConcurrentCallsShareOneLogin(t *testing.T) {
	f := &fakeTino{checkStatus: "pending"}
	a := newAdapter(t, f)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if _, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "INV-1"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := f.logins.Load(); n != 1 {
		t.Fatalf("logins = %d, want 1", n)
	}
}

func TestCancelInvoice(t *testing.T) {
	a := newAdapter(t, &fakeTino{})
	if err := a.CancelInvoice(context.Background(), core.CancelInvoiceInput{InvoiceID: "INV-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestMissingInvoiceIDIsInvalidInput(t *testing.T) {
	a := newAdapter(t, &fakeTino{})
	ctx := context.Background()
	if _, err := a.CheckInvoice(ctx, core.CheckInvoiceInput{OrderID: "order-1"}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("check: want ErrInvalidInput, got %v", err)
	}
	if err := a.CancelInvoice(ctx, core.CancelInvoiceInput{}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("cancel: want ErrInvalidInput, got %v", err)
	}
}

func TestProviderErrorIsWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"status": false, "message": "invalid credentials"})
	}))
	t.Cleanup(srv.Close)
	a, err := tino.New(tino.Config{AuthURL: srv.URL, BaseURL: srv.URL, Username: "u", Password: "p", WalletID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o", Amount: 1})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Provider != core.ProviderTino || pe.Op != "create_invoice" {
		t.Fatalf("want ProviderError{tino, create_invoice}, got %v", err)
	}
}
