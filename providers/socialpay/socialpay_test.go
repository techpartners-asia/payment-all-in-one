package socialpay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

type fakeSocialPay struct {
	*httptest.Server
	requests map[string]map[string]string
}

// newFakeSocialPay serves each path's reply inside SocialPay's
// {"header": ..., "body": {"response"|"error": ...}} envelope.
func newFakeSocialPay(t *testing.T, replies map[string]envelope) *fakeSocialPay {
	t.Helper()
	f := &fakeSocialPay{requests: map[string]map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("%s decode: %v", r.URL.Path, err)
		}
		f.requests[r.URL.Path] = req
		reply, ok := replies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(f.Close)
	return f
}

type envelope struct {
	Header struct {
		Status string `json:"status"`
		Code   int    `json:"code"`
	} `json:"header"`
	Body struct {
		Response map[string]any `json:"response,omitempty"`
		Error    map[string]any `json:"error,omitempty"`
	} `json:"body"`
}

func ok(response map[string]any) envelope {
	var e envelope
	e.Header.Status, e.Header.Code = "SUCCESS", 200
	e.Body.Response = response
	return e
}

func fail(desc string) envelope {
	var e envelope
	e.Header.Status, e.Header.Code = "FAIL", 400
	e.Body.Error = map[string]any{"errorDesc": desc, "errorType": "INVOICE"}
	return e
}

func newTestAdapter(t *testing.T, endpoint string) *Adapter {
	t.Helper()
	a, err := New(Config{Terminal: "T001", Secret: "s3cret", Endpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNewRequiresConfig(t *testing.T) {
	_, err := New(Config{Terminal: "T001"})
	if !errors.Is(err, core.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestCreateInvoiceQRWhenNoPhone(t *testing.T) {
	const qr = "000201010212153137944992580085800000000058264040014A00000084300011234"
	srv := newFakeSocialPay(t, map[string]envelope{
		"/pos/invoice/qr": ok(map[string]any{"desc": qr, "status": "SUCCESS"}),
	})
	a := newTestAdapter(t, srv.URL)

	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "order-1", Amount: 1500})
	if err != nil {
		t.Fatal(err)
	}
	sent := srv.requests["/pos/invoice/qr"]
	if sent["invoice"] != "order-1" || sent["terminal"] != "T001" || sent["amount"] != "1500.00" {
		t.Errorf("request = %v", sent)
	}
	if _, used := srv.requests["/pos/invoice/phone"]; used {
		t.Error("phone endpoint called for a QR invoice")
	}
	if inv.QRText != qr || inv.InvoiceID != "order-1" || inv.Status != core.StatusPending {
		t.Errorf("invoice = %+v", inv)
	}
}

func TestCreateInvoicePhoneWhenPhoneSet(t *testing.T) {
	srv := newFakeSocialPay(t, map[string]envelope{
		"/pos/invoice/phone": ok(map[string]any{"desc": "Invoice sent", "status": "SUCCESS"}),
	})
	a := newTestAdapter(t, srv.URL)

	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "order-2", Amount: 2000, Phone: "99112233"})
	if err != nil {
		t.Fatal(err)
	}
	sent := srv.requests["/pos/invoice/phone"]
	if sent["phone"] != "99112233" || sent["invoice"] != "order-2" {
		t.Errorf("request = %v", sent)
	}
	if _, used := srv.requests["/pos/invoice/qr"]; used {
		t.Error("QR endpoint called for a phone invoice")
	}
	if inv.QRText != "" {
		t.Errorf("QRText = %q, want empty for a phone invoice", inv.QRText)
	}
}

func TestCreateInvoiceRefused(t *testing.T) {
	cases := map[string]envelope{
		"error header":   fail("Terminal not found"),
		"refused status": ok(map[string]any{"desc": "Duplicate invoice", "status": "FAIL"}),
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newFakeSocialPay(t, map[string]envelope{"/pos/invoice/qr": reply})
			a := newTestAdapter(t, srv.URL)
			_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o", Amount: 1})
			var pe *core.ProviderError
			if !errors.As(err, &pe) || pe.Op != "create_invoice" {
				t.Fatalf("err = %v, want create_invoice ProviderError", err)
			}
		})
	}
}

func TestCheckInvoicePaid(t *testing.T) {
	srv := newFakeSocialPay(t, map[string]envelope{
		"/pos/invoice/check": ok(map[string]any{
			"approval_code": "A12345",
			"amount":        "1500.00",
			"card_number":   "4111****1111",
			"resp_desc":     "Approved",
			"resp_code":     "00",
			"terminal":      "T001",
			"invoice":       "order-1",
			"checksum":      "ignored",
		}),
	})
	a := newTestAdapter(t, srv.URL)

	st, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "order-1", Amount: 1500})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != core.StatusPaid || st.PaidAmount != 1500 || st.TransactionID != "A12345" || st.InvoiceID != "order-1" {
		t.Errorf("status = %+v", st)
	}
}

func TestCheckInvoiceUnpaidIsProviderError(t *testing.T) {
	srv := newFakeSocialPay(t, map[string]envelope{"/pos/invoice/check": fail("Invoice not paid")})
	a := newTestAdapter(t, srv.URL)

	_, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{OrderID: "order-1", Amount: 1500})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "check_invoice" {
		t.Fatalf("err = %v, want check_invoice ProviderError", err)
	}
}

func TestCheckInvoiceInvalidInput(t *testing.T) {
	a := newTestAdapter(t, "http://unused.invalid")
	for name, in := range map[string]core.CheckInvoiceInput{
		"no id":     {Amount: 1},
		"no amount": {InvoiceID: "order-1"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := a.CheckInvoice(context.Background(), in); !errors.Is(err, core.ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestPaymentStatus(t *testing.T) {
	for code, want := range map[string]core.Status{"00": core.StatusPaid, "": core.StatusPending, "05": core.StatusPending} {
		if got := paymentStatus(code); got != want {
			t.Errorf("paymentStatus(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestRefund(t *testing.T) {
	srv := newFakeSocialPay(t, map[string]envelope{
		"/pos/payment/cancel": ok(map[string]any{"resp_code": "00", "resp_desc": "Approved", "invoice": "order-1", "amount": "1500.00"}),
	})
	a := newTestAdapter(t, srv.URL)

	if err := a.Refund(context.Background(), core.RefundInput{InvoiceID: "order-1", Amount: 1500}); err != nil {
		t.Fatal(err)
	}
	if sent := srv.requests["/pos/payment/cancel"]; sent["invoice"] != "order-1" || sent["amount"] != "1500.00" {
		t.Errorf("request = %v", sent)
	}
}

func TestRefundNotApproved(t *testing.T) {
	srv := newFakeSocialPay(t, map[string]envelope{
		"/pos/payment/cancel": ok(map[string]any{"resp_code": "12", "resp_desc": "Invalid transaction"}),
	})
	a := newTestAdapter(t, srv.URL)

	var pe *core.ProviderError
	if err := a.Refund(context.Background(), core.RefundInput{InvoiceID: "order-1", Amount: 1500}); !errors.As(err, &pe) || pe.Op != "refund" {
		t.Fatalf("err = %v, want refund ProviderError", err)
	}
}

func TestRefundRequiresAmount(t *testing.T) {
	a := newTestAdapter(t, "http://unused.invalid")
	if err := a.Refund(context.Background(), core.RefundInput{InvoiceID: "order-1"}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestCancelInvoice(t *testing.T) {
	srv := newFakeSocialPay(t, map[string]envelope{
		"/pos/invoice/cancel": ok(map[string]any{"desc": "Invoice cancelled", "status": "SUCCESS"}),
	})
	a := newTestAdapter(t, srv.URL)

	if err := a.CancelInvoice(context.Background(), core.CancelInvoiceInput{OrderID: "order-1", Amount: 1500}); err != nil {
		t.Fatal(err)
	}
	if sent := srv.requests["/pos/invoice/cancel"]; sent["invoice"] != "order-1" || sent["amount"] != "1500.00" || sent["checksum"] == "" {
		t.Errorf("request = %v", sent)
	}
}

func TestCancelInvoiceRejected(t *testing.T) {
	srv := newFakeSocialPay(t, map[string]envelope{
		"/pos/invoice/cancel": ok(map[string]any{"desc": "Already paid", "status": "FAIL"}),
	})
	a := newTestAdapter(t, srv.URL)

	var pe *core.ProviderError
	if err := a.CancelInvoice(context.Background(), core.CancelInvoiceInput{InvoiceID: "order-1", Amount: 1500}); !errors.As(err, &pe) || pe.Op != "cancel_invoice" {
		t.Fatalf("err = %v, want cancel_invoice ProviderError", err)
	}
	if err := a.CancelInvoice(context.Background(), core.CancelInvoiceInput{InvoiceID: "order-1"}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("missing amount: err = %v, want ErrInvalidInput", err)
	}
}
