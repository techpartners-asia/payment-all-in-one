package monpay

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

type fakeMonpay struct {
	lastGenerate map[string]any
	checkBody    string
}

func newTestAdapter(t *testing.T) (*Adapter, *fakeMonpay) {
	t.Helper()
	fake := &fakeMonpay{}
	mux := http.NewServeMux()
	authed := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if user, pass, _ := r.BasicAuth(); user != "branch" || pass != "acc-1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("POST /rest/branch/qrpurchase/generate", authed(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &fake.lastGenerate); err != nil {
			t.Errorf("generate body: %v", err)
		}
		io.WriteString(w, `{"code":0,"info":"Success","result":{"qrcode":"MPQR:abc123","uuid":"uuid-1"}}`)
	}))
	mux.HandleFunc("GET /rest/branch/qrpurchase/check", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("uuid") != "uuid-1" {
			t.Errorf("check uuid = %q", r.URL.Query().Get("uuid"))
		}
		io.WriteString(w, fake.checkBody)
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a, err := New(Config{Endpoint: srv.URL, Username: "branch", AccountID: "acc-1", CallbackURL: "https://merchant.example/monpay"})
	if err != nil {
		t.Fatal(err)
	}
	return a, fake
}

func TestNewRequiresCredentials(t *testing.T) {
	_, err := New(Config{Endpoint: "https://z-wallet.monpay.mn"})
	if !errors.Is(err, core.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "Username") || !strings.Contains(err.Error(), "AccountID") {
		t.Errorf("error %q does not name missing fields", err)
	}
}

func TestCreateInvoiceGeneratesQR(t *testing.T) {
	a, fake := newTestAdapter(t)
	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "order-1", Amount: 2500})
	if err != nil {
		t.Fatal(err)
	}
	if inv.InvoiceID != "uuid-1" || inv.QRText != "MPQR:abc123" || inv.OrderID != "order-1" || inv.Status != core.StatusPending {
		t.Errorf("invoice = %+v", inv)
	}
	if fake.lastGenerate["amount"] != float64(2500) || fake.lastGenerate["callbackUrl"] != "https://merchant.example/monpay" || fake.lastGenerate["generateUuid"] != true {
		t.Errorf("generate request = %v", fake.lastGenerate)
	}
}

func TestCreateInvoiceCallbackOverride(t *testing.T) {
	a, fake := newTestAdapter(t)
	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{Amount: 100, CallbackURL: "https://merchant.example/other"})
	if err != nil {
		t.Fatal(err)
	}
	if fake.lastGenerate["callbackUrl"] != "https://merchant.example/other" {
		t.Errorf("callbackUrl = %v", fake.lastGenerate["callbackUrl"])
	}
}

func TestCreateInvoiceRejectsNonPositiveAmount(t *testing.T) {
	a, _ := newTestAdapter(t)
	if _, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{Amount: 0}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
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
			body:       `{"code":0,"info":"Success","result":{"uuid":"uuid-1","usedAt":1731740000000,"usedById":42,"transactionId":"txn-77","amount":2500,"createdAt":1731739000000,"userPhone":"99112233"}}`,
			wantStatus: core.StatusPaid, wantAmount: 2500, wantTxn: "txn-77",
		},
		{
			name:       "not scanned yet",
			body:       `{"code":23,"info":"QR not scanned","result":{"uuid":"uuid-1"}}`,
			wantStatus: core.StatusPending,
		},
		{
			name:       "success without payment evidence",
			body:       `{"code":0,"info":"Success","result":{"uuid":"uuid-1","amount":2500}}`,
			wantStatus: core.StatusPending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, fake := newTestAdapter(t)
			fake.checkBody = tt.body
			st, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "uuid-1"})
			if err != nil {
				t.Fatal(err)
			}
			if st.Status != tt.wantStatus || st.PaidAmount != tt.wantAmount || st.TransactionID != tt.wantTxn {
				t.Errorf("status = %+v", st)
			}
		})
	}
}

func TestCheckInvoiceProviderError(t *testing.T) {
	a, fake := newTestAdapter(t)
	fake.checkBody = `{"code":999,"info":"internal error"}`
	_, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "uuid-1"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "check_invoice" || !strings.Contains(err.Error(), "code=999") {
		t.Fatalf("err = %v, want wrapped check_invoice error carrying code", err)
	}
}

func TestCheckInvoiceRequiresInvoiceID(t *testing.T) {
	a, _ := newTestAdapter(t)
	if _, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}
