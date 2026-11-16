package golomt

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/techpartners-asia/golomt-api-go/utils"
	"github.com/techpartners-asia/payment-all-in-one/core"
)

const (
	testSecret = "s3cret"
	testBearer = "bearer-token"
)

type fakeGolomt struct {
	*httptest.Server
	requests map[string]map[string]string
}

func newFakeGolomt(t *testing.T, handlers map[string]func(req map[string]string) any) *fakeGolomt {
	t.Helper()
	f := &fakeGolomt{requests: map[string]map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testBearer {
			t.Errorf("%s Authorization = %q", r.URL.Path, got)
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("%s decode: %v", r.URL.Path, err)
		}
		f.requests[r.URL.Path] = req
		h, ok := handlers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h(req))
	}))
	t.Cleanup(f.Close)
	return f
}

func newTestAdapter(t *testing.T, baseURL string) *Adapter {
	t.Helper()
	a, err := New(Config{BaseURL: baseURL + "/", Secret: testSecret, BearerToken: testBearer, CallbackURL: "https://shop.example/default"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNewRequiresCredentials(t *testing.T) {
	_, err := New(Config{})
	if !errors.Is(err, core.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestCreateInvoice(t *testing.T) {
	srv := newFakeGolomt(t, map[string]func(map[string]string) any{
		"/api/invoice": func(req map[string]string) any {
			return map[string]string{
				"invoice":        "INV-77",
				"transactionId":  req["transactionId"],
				"checksum":       utils.GenerateHMAC(testSecret, "INV-77"+req["transactionId"]),
				"socialDeeplink": "socialpay-payment://key=INV-77",
			}
		},
	})
	a := newTestAdapter(t, srv.URL)

	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID:     "order-1",
		Amount:      1500,
		CallbackURL: "https://shop.example/return",
		Extra: map[string]string{
			"return_type":     "mobile",
			"lang":            "EN",
			"payment_method":  "socialpay",
			"get_token":       "true",
			"social_deeplink": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	sent := srv.requests["/api/invoice"]
	wantSent := map[string]string{
		"amount":         "1500.00",
		"transactionId":  "order-1",
		"returnType":     "MOBILE",
		"callback":       "https://shop.example/return",
		"genToken":       "Y",
		"socialDeeplink": "Y",
	}
	for k, v := range wantSent {
		if sent[k] != v {
			t.Errorf("request %s = %q, want %q", k, sent[k], v)
		}
	}

	if inv.InvoiceID != "order-1" || inv.OrderID != "order-1" || inv.Amount != 1500 {
		t.Errorf("ids/amount = %q %q %v", inv.InvoiceID, inv.OrderID, inv.Amount)
	}
	if want := srv.URL + "/socialpay/EN/INV-77"; inv.PaymentURL != want {
		t.Errorf("PaymentURL = %q, want %q", inv.PaymentURL, want)
	}
	if len(inv.Deeplinks) != 1 || inv.Deeplinks[0].Link != "socialpay-payment://key=INV-77" {
		t.Errorf("Deeplinks = %+v", inv.Deeplinks)
	}
	if inv.Status != core.StatusPending {
		t.Errorf("Status = %q", inv.Status)
	}
}

func TestCreateInvoiceDefaults(t *testing.T) {
	srv := newFakeGolomt(t, map[string]func(map[string]string) any{
		"/api/invoice": func(req map[string]string) any {
			return map[string]string{
				"invoice":       "INV-1",
				"transactionId": req["transactionId"],
				"checksum":      utils.GenerateHMAC(testSecret, "INV-1"+req["transactionId"]),
			}
		},
	})
	a := newTestAdapter(t, srv.URL)

	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "order-2", Amount: 10})
	if err != nil {
		t.Fatal(err)
	}
	sent := srv.requests["/api/invoice"]
	if sent["returnType"] != "GET" || sent["callback"] != "https://shop.example/default" || sent["genToken"] != "N" {
		t.Errorf("request = %v", sent)
	}
	if want := srv.URL + "/payment/MN/INV-1"; inv.PaymentURL != want {
		t.Errorf("PaymentURL = %q, want %q", inv.PaymentURL, want)
	}
	if inv.Deeplinks != nil {
		t.Errorf("Deeplinks = %+v, want none", inv.Deeplinks)
	}
}

func TestCreateInvoiceRejectedByGolomt(t *testing.T) {
	srv := newFakeGolomt(t, map[string]func(map[string]string) any{
		"/api/invoice": func(req map[string]string) any {
			return map[string]string{
				"transactionId": req["transactionId"],
				"checksum":      utils.GenerateHMAC(testSecret, req["transactionId"]),
				"errorCode":     "103",
				"errorDesc":     "duplicate transaction",
			}
		},
	})
	a := newTestAdapter(t, srv.URL)

	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "order-3", Amount: 10})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "create_invoice" {
		t.Fatalf("err = %v, want create_invoice ProviderError", err)
	}
}

func TestCreateInvoiceInvalidInput(t *testing.T) {
	a := newTestAdapter(t, "http://unused.invalid")
	cases := map[string]core.CreateInvoiceInput{
		"bad return_type": {OrderID: "o", Amount: 1, Extra: map[string]string{"return_type": "PUT"}},
		"bad lang":        {OrderID: "o", Amount: 1, Extra: map[string]string{"lang": "RU"}},
		"bad method":      {OrderID: "o", Amount: 1, Extra: map[string]string{"payment_method": "cash"}},
		"bad flag":        {OrderID: "o", Amount: 1, Extra: map[string]string{"get_token": "yes please"}},
		"no order":        {Amount: 1},
		"zero amount":     {OrderID: "o"},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := a.CreateInvoice(context.Background(), in); !errors.Is(err, core.ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}

	noCallback, err := New(Config{Secret: testSecret, BearerToken: testBearer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noCallback.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o", Amount: 1}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("missing callback err = %v, want ErrInvalidInput", err)
	}
}

func TestCheckInvoicePaid(t *testing.T) {
	srv := newFakeGolomt(t, map[string]func(map[string]string) any{
		"/api/inquiry": func(req map[string]string) any {
			return map[string]string{
				"amount":        "1500.00",
				"errorCode":     "000",
				"errorDesc":     "Гүйлгээ амжилттай",
				"status":        "SENT",
				"transactionId": req["transactionId"],
				"cardNumber":    "4111****1111",
				"checksum":      utils.GenerateHMAC(testSecret, req["transactionId"]+"000"+"1500.00"),
			}
		},
	})
	a := newTestAdapter(t, srv.URL)

	st, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "order-1"})
	if err != nil {
		t.Fatal(err)
	}
	if srv.requests["/api/inquiry"]["transactionId"] != "order-1" {
		t.Errorf("inquiry request = %v", srv.requests["/api/inquiry"])
	}
	if st.Status != core.StatusPaid || st.PaidAmount != 1500 || st.InvoiceID != "order-1" {
		t.Errorf("status = %+v", st)
	}
}

func TestCheckInvoiceUnpaidIsProviderError(t *testing.T) {
	srv := newFakeGolomt(t, map[string]func(map[string]string) any{
		"/api/inquiry": func(req map[string]string) any {
			return map[string]string{"errorCode": "101", "errorDesc": "not paid", "transactionId": req["transactionId"]}
		},
	})
	a := newTestAdapter(t, srv.URL)

	_, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{OrderID: "order-1"})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "check_invoice" {
		t.Fatalf("err = %v, want check_invoice ProviderError", err)
	}
}

func TestCheckInvoiceRequiresID(t *testing.T) {
	a := newTestAdapter(t, "http://unused.invalid")
	if _, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestInquiryStatus(t *testing.T) {
	cases := map[string]core.Status{"000": core.StatusPaid, "": core.StatusPending, "101": core.StatusPending}
	for code, want := range cases {
		if got := inquiryStatus(code); got != want {
			t.Errorf("inquiryStatus(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestRefund(t *testing.T) {
	srv := newFakeGolomt(t, map[string]func(map[string]string) any{
		"/payment/void": func(req map[string]string) any {
			return map[string]string{"statusCode": "000", "desc": "success", "txnID": req["txnId"]}
		},
	})
	a := newTestAdapter(t, srv.URL)

	if err := a.Refund(context.Background(), core.RefundInput{InvoiceID: "order-1"}); err != nil {
		t.Fatal(err)
	}
	if srv.requests["/payment/void"]["txnId"] != "order-1" {
		t.Errorf("void request = %v", srv.requests["/payment/void"])
	}
}

func TestRefundRejectsPartial(t *testing.T) {
	a := newTestAdapter(t, "http://unused.invalid")
	if err := a.Refund(context.Background(), core.RefundInput{InvoiceID: "order-1", Amount: 5}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestRefundDeclined(t *testing.T) {
	srv := newFakeGolomt(t, map[string]func(map[string]string) any{
		"/payment/void": func(map[string]string) any {
			return map[string]string{"statusCode": "107", "desc": "already voided"}
		},
	})
	a := newTestAdapter(t, srv.URL)
	var pe *core.ProviderError
	if err := a.Refund(context.Background(), core.RefundInput{OrderID: "order-1"}); !errors.As(err, &pe) || pe.Op != "refund" {
		t.Fatalf("err = %v, want refund ProviderError", err)
	}
}
