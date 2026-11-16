package bonum_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/techpartners-asia/payment-all-in-one/core"
	"github.com/techpartners-asia/payment-all-in-one/providers/bonum"
)

const (
	ecommercePath = "/bonum-gateway/ecommerce"
	mpayPath      = "/mpay-service/merchant"
)

type fakeBonum struct {
	mu   sync.Mutex
	sent map[string]map[string]any
	// htmlReplies makes invoice endpoints answer like a misconfigured proxy.
	htmlReplies bool
}

func (f *fakeBonum) body(path string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[path]
}

func (f *fakeBonum) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == ecommercePath+"/auth/create" {
		if r.Header.Get("Authorization") != "AppSecret secret-1" || r.Header.Get("X-TERMINAL-ID") != "17171119" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"traceId":"t-401","message":"bad secret","status":401}`))
			return
		}
		_, _ = w.Write([]byte(`{"tokenType":"Bearer","accessToken":"access-1","expiresIn":1800,"refreshToken":"refresh-1","refreshExpiresIn":3600,"unit":"SECONDS"}`))
		return
	}
	if r.Header.Get("Authorization") != "Bearer access-1" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.sent[r.URL.Path] = body
	html := f.htmlReplies
	f.mu.Unlock()

	if html {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html>maintenance</html>`))
		return
	}
	switch r.URL.Path {
	case mpayPath + "/transaction/qr/create":
		_, _ = w.Write([]byte(`{"traceId":"t-qr","message":"ok","status":200,"data":{"invoiceId":"qr-1","qrCode":"0002010102QR","qrImage":"iVBORw0KGgo","links":[{"name":"Khan Bank","description":"Khan Bank app","logo":"https://qpay.mn/q/logo/khanbank.png","link":"khanbank://q?qPay_QRcode=0002","appStoreId":"1555908766","androidPackageName":"com.khanbank.retail"}]}}`))
	case ecommercePath + "/invoices":
		_, _ = w.Write([]byte(`{"invoiceId":"inv-1","followUpLink":"https://ecommerce.bonum.mn/ecommerce?invoiceId=inv-1"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"traceId":"t-404","message":"not found","status":404}`))
	}
}

func newAdapter(t *testing.T, cfg bonum.Config) (*fakeBonum, *bonum.Adapter) {
	t.Helper()
	f := &fakeBonum{sent: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg.AppSecret, cfg.TerminalID, cfg.BaseURL = "secret-1", "17171119", srv.URL
	a, err := bonum.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f, a
}

func TestNewRejectsMissingConfig(t *testing.T) {
	_, err := bonum.New(bonum.Config{AppSecret: "s"})
	if !errors.Is(err, core.ErrInvalidConfig) || !strings.Contains(err.Error(), "TerminalID") {
		t.Fatalf("want ErrInvalidConfig naming TerminalID, got %v", err)
	}
}

func TestCreateInvoiceDefaultsToQR(t *testing.T) {
	f, a := newAdapter(t, bonum.Config{})
	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "order-1", Amount: 500})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Provider != core.ProviderBonum || inv.InvoiceID != "qr-1" || inv.OrderID != "order-1" || inv.Amount != 500 ||
		inv.QRText != "0002010102QR" || inv.QRImage != "iVBORw0KGgo" || inv.PaymentURL != "" ||
		inv.Status != core.StatusPending || inv.Raw == nil {
		t.Fatalf("invoice = %+v", inv)
	}
	wantLink := core.Deeplink{Name: "Khan Bank", Description: "Khan Bank app", Logo: "https://qpay.mn/q/logo/khanbank.png", Link: "khanbank://q?qPay_QRcode=0002"}
	if len(inv.Deeplinks) != 1 || inv.Deeplinks[0] != wantLink {
		t.Fatalf("deeplinks = %+v", inv.Deeplinks)
	}
	sent := f.body(mpayPath + "/transaction/qr/create")
	if sent["transactionId"] != "order-1" || sent["amount"] != float64(500) || sent["expiresIn"] != float64(900) {
		t.Fatalf("request body = %v", sent)
	}
}

func TestCreateInvoiceCheckoutFlow(t *testing.T) {
	f, a := newAdapter(t, bonum.Config{ReturnURL: "https://shop.example/default-return"})
	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID:   "order-2",
		Amount:    15000,
		ReturnURL: "https://shop.example/orders/2",
		Extra:     map[string]string{"flow": "checkout", "providers": "QPAY, E_COMMERCE"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inv.InvoiceID != "inv-1" || inv.PaymentURL != "https://ecommerce.bonum.mn/ecommerce?invoiceId=inv-1" ||
		inv.QRText != "" || inv.Status != core.StatusPending || inv.OrderID != "order-2" {
		t.Fatalf("invoice = %+v", inv)
	}
	sent := f.body(ecommercePath + "/invoices")
	providers, _ := sent["providers"].([]any)
	if sent["callback"] != "https://shop.example/orders/2" || sent["transactionId"] != "order-2" ||
		len(providers) != 2 || providers[0] != "QPAY" || providers[1] != "E_COMMERCE" {
		t.Fatalf("request body = %v", sent)
	}
}

func TestCheckoutRequiresReturnURL(t *testing.T) {
	_, a := newAdapter(t, bonum.Config{})
	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID: "order-3", Amount: 100, Extra: map[string]string{"flow": "checkout"},
	})
	if !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}

func TestUnknownFlowIsInvalidInput(t *testing.T) {
	_, a := newAdapter(t, bonum.Config{})
	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID: "order-4", Amount: 100, Extra: map[string]string{"flow": "card"},
	})
	if !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}

func TestSDKErrorIsWrapped(t *testing.T) {
	srv := httptest.NewServer(&fakeBonum{sent: map[string]map[string]any{}})
	t.Cleanup(srv.Close)
	a, err := bonum.New(bonum.Config{AppSecret: "wrong", TerminalID: "17171119", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o", Amount: 1})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Provider != core.ProviderBonum || pe.Op != "create_invoice" {
		t.Fatalf("want ProviderError{bonum, create_invoice}, got %v", err)
	}
}

func TestCheckInvoiceIsUnsupported(t *testing.T) {
	_, a := newAdapter(t, bonum.Config{})
	_, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "qr-1"})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

func TestUndecodableCreateResponseIsAnError(t *testing.T) {
	for _, flow := range []string{"qr", "checkout"} {
		t.Run(flow, func(t *testing.T) {
			f, a := newAdapter(t, bonum.Config{ReturnURL: "https://shop.test/return"})
			f.htmlReplies = true
			_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
				OrderID: "order-1", Amount: 1000, Extra: map[string]string{"flow": flow},
			})
			var pe *core.ProviderError
			if !errors.As(err, &pe) || pe.Op != "create_invoice" {
				t.Fatalf("err = %v, want create_invoice ProviderError", err)
			}
		})
	}
}
