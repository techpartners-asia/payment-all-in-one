package balc

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

type fakeBalc struct {
	mu        sync.Mutex
	limit     map[string]any
	loanReply string
	calls     []string
	loanBody  []map[string]any
	custIDs   []string
}

func (f *fakeBalc) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		fn := r.Header.Get("func")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, fn)
		f.custIDs = append(f.custIDs, r.URL.Query().Get("cust_id"))
		w.Header().Set("Content-Type", "application/json")
		switch fn {
		case "limitcheck":
			_ = json.NewEncoder(w).Encode(f.limit)
		case "loanadv":
			if err := json.NewDecoder(r.Body).Decode(&f.loanBody); err != nil {
				t.Errorf("decode loan body: %v", err)
			}
			_, _ = w.Write([]byte(f.loanReply))
		default:
			http.Error(w, "unknown func", http.StatusBadRequest)
		}
	})
}

func newTestAdapter(t *testing.T, f *fakeBalc) *Adapter {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	a, err := New(Config{Endpoint: srv.URL, Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func usableLimit(avail float64) map[string]any {
	return map[string]any{"totalLimit": 1000000, "usedLimit": 1000000 - avail, "availLimit": avail, "status": 1}
}

func TestNewRequiresConfig(t *testing.T) {
	_, err := New(Config{Endpoint: "https://balc.test"})
	if !errors.Is(err, core.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestCreateInvoiceDrawsLoanAndIsPaid(t *testing.T) {
	f := &fakeBalc{limit: usableLimit(500000), loanReply: `"ACC-001"`}
	a := newTestAdapter(t, f)

	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID:    "order-1",
		Amount:     120000,
		CustomerID: "42",
	})
	if err != nil {
		t.Fatal(err)
	}

	if inv.InvoiceID != "ACC-001" || inv.Status != core.StatusPaid || inv.OrderID != "order-1" || inv.Amount != 120000 {
		t.Errorf("invoice = %+v", inv)
	}
	if len(f.calls) != 2 || f.calls[0] != "limitcheck" || f.calls[1] != "loanadv" {
		t.Errorf("calls = %v", f.calls)
	}
	if f.custIDs[1] != "42" {
		t.Errorf("cust_id = %q", f.custIDs[1])
	}
	if len(f.loanBody) != 1 || f.loanBody[0]["amt"] != float64(120000) || f.loanBody[0]["description"] != "order-1" {
		t.Errorf("loan body = %v", f.loanBody)
	}
}

func TestCreateInvoiceRefusesWithoutCredit(t *testing.T) {
	cases := map[string]map[string]any{
		"limit below amount": usableLimit(1000),
		"line disabled":      {"totalLimit": 1000000, "availLimit": 1000000, "status": 0},
	}
	for name, limit := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeBalc{limit: limit, loanReply: `"ACC-001"`}
			a := newTestAdapter(t, f)

			_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o1", Amount: 5000, CustomerID: "42"})
			if !errors.Is(err, ErrCreditUnavailable) {
				t.Fatalf("err = %v, want ErrCreditUnavailable", err)
			}
			if len(f.calls) != 1 {
				t.Errorf("calls = %v, want only limitcheck", f.calls)
			}
		})
	}
}

func TestCreateInvoiceRejectsNonStringLoanReply(t *testing.T) {
	f := &fakeBalc{limit: usableLimit(500000), loanReply: `{"error":"customer blocked"}`}
	a := newTestAdapter(t, f)

	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o1", Amount: 5000, CustomerID: "42"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || inv != nil {
		t.Fatalf("inv = %+v, err = %v, want ProviderError and no invoice", inv, err)
	}
}

func TestCreateInvoiceValidatesInput(t *testing.T) {
	a := newTestAdapter(t, &fakeBalc{})
	for name, in := range map[string]core.CreateInvoiceInput{
		"missing customer":     {Amount: 5000},
		"non-numeric customer": {Amount: 5000, CustomerID: "abc"},
		"fractional amount":    {Amount: 10.5, CustomerID: "42"},
	} {
		if _, err := a.CreateInvoice(context.Background(), in); !errors.Is(err, core.ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
}

func TestCheckInvoiceUnsupported(t *testing.T) {
	a := newTestAdapter(t, &fakeBalc{})
	_, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "ACC-001"})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestTransportFailureReturnsError(t *testing.T) {
	a, err := New(Config{Endpoint: "http://127.0.0.1:1", Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o1", Amount: 5000, CustomerID: "42"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want ProviderError", err)
	}
}
