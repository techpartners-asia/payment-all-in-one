package pocket

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/techpartners-asia/payment-all-in-one/core"
	pocketsdk "github.com/techpartners-asia/pocket-go"
)

// fakePocket stands in for pocket-go's client, whose hosts are hardcoded.
// It issues tok-1, tok-2, ... on each Login and accepts only tokens for which
// accepts returns true, mimicking pocket-go's ErrUnauthorized on rejection.
type fakePocket struct {
	clock     *time.Time
	lifetime  time.Duration
	accepts   func(token string) bool
	loginErr  error
	logins    int
	token     pocketsdk.Token
	created   []pocketsdk.PocketCreateInvoiceInput
	createRes pocketsdk.PocketCreateInvoiceResponse
	detail    pocketsdk.PocketInvoiceDetailResponse
	lookedUp  string
}

func (f *fakePocket) Login(context.Context) (pocketsdk.Token, error) {
	if f.loginErr != nil {
		return pocketsdk.Token{}, f.loginErr
	}
	f.logins++
	t := pocketsdk.Token{AccessToken: fmt.Sprintf("tok-%d", f.logins)}
	if f.lifetime > 0 {
		t.ExpiresAt = f.clock.Add(f.lifetime)
	}
	return t, nil
}

func (f *fakePocket) SetToken(t pocketsdk.Token) { f.token = t }
func (f *fakePocket) Token() pocketsdk.Token     { return f.token }
func (f *fakePocket) Close()                     { f.token = pocketsdk.Token{} }

func (f *fakePocket) authorize() error {
	if f.token.IsZero() {
		return pocketsdk.ErrNoToken
	}
	if f.accepts != nil && !f.accepts(f.token.AccessToken) {
		return fmt.Errorf("%w (Status: 401)", pocketsdk.ErrUnauthorized)
	}
	return nil
}

func (f *fakePocket) CreateInvoice(in pocketsdk.PocketCreateInvoiceInput) (pocketsdk.PocketCreateInvoiceResponse, error) {
	if err := f.authorize(); err != nil {
		return pocketsdk.PocketCreateInvoiceResponse{}, err
	}
	f.created = append(f.created, in)
	return f.createRes, nil
}

func (f *fakePocket) GetInvoiceByInvoiceID(id string) (pocketsdk.PocketInvoiceDetailResponse, error) {
	if err := f.authorize(); err != nil {
		return pocketsdk.PocketInvoiceDetailResponse{}, err
	}
	f.lookedUp = "invoice:" + id
	return f.detail, nil
}

func (f *fakePocket) GetInvoiceByOrderNumber(order string) (pocketsdk.PocketInvoiceDetailResponse, error) {
	if err := f.authorize(); err != nil {
		return pocketsdk.PocketInvoiceDetailResponse{}, err
	}
	f.lookedUp = "order:" + order
	return f.detail, nil
}

func newFake() (*fakePocket, *Adapter) {
	clock := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	f := &fakePocket{
		clock:     &clock,
		lifetime:  5 * time.Minute,
		createRes: pocketsdk.PocketCreateInvoiceResponse{ID: 4821, Qr: "pocket-qr-data", OrderNumber: "order-1", DeepLink: "pocket://invoice/4821"},
	}
	return f, newAdapter(f, func() time.Time { return *f.clock })
}

func TestNewValidatesConfig(t *testing.T) {
	valid := Config{ClientID: "id", ClientSecret: "secret", TerminalID: 74686381183671}
	if _, err := New(valid); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	cases := map[string]func(*Config){
		"no client id":    func(c *Config) { c.ClientID = "" },
		"no secret":       func(c *Config) { c.ClientSecret = "" },
		"no terminal":     func(c *Config) { c.TerminalID = 0 },
		"bad environment": func(c *Config) { c.Environment = "staging" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if _, err := New(cfg); !errors.Is(err, core.ErrInvalidConfig) {
				t.Fatalf("err = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestCreateInvoice(t *testing.T) {
	f, a := newFake()

	inv, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "order-1", Amount: 10000})
	if err != nil {
		t.Fatal(err)
	}
	want := pocketsdk.PocketCreateInvoiceInput{Amount: 10000, Info: "order-1", OrderNumber: "order-1", InvoiceType: "ZERO", Channel: "ecommerce"}
	if len(f.created) != 1 || f.created[0] != want {
		t.Errorf("sent %+v, want %+v", f.created, want)
	}
	if inv.InvoiceID != "4821" || inv.OrderID != "order-1" || inv.QRText != "pocket-qr-data" || inv.Status != core.StatusPending {
		t.Errorf("invoice = %+v", inv)
	}
	if len(inv.Deeplinks) != 1 || inv.Deeplinks[0].Link != "pocket://invoice/4821" {
		t.Errorf("Deeplinks = %+v", inv.Deeplinks)
	}
}

func TestCreateInvoiceExtra(t *testing.T) {
	f, a := newFake()
	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{
		OrderID: "order-1", Amount: 500, Description: "Coffee",
		Extra: map[string]string{"invoice_type": "purchase", "channel": "pos"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.created[0]; got.InvoiceType != "PURCHASE" || got.Channel != "pos" || got.Info != "Coffee" {
		t.Errorf("sent %+v", got)
	}
}

func TestCreateInvoiceInvalidInput(t *testing.T) {
	_, a := newFake()
	for name, in := range map[string]core.CreateInvoiceInput{
		"no order":         {Amount: 1},
		"zero amount":      {OrderID: "o"},
		"bad invoice_type": {OrderID: "o", Amount: 1, Extra: map[string]string{"invoice_type": "LOAN"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := a.CreateInvoice(context.Background(), in); !errors.Is(err, core.ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestCreateInvoiceEmptyResponseIsError(t *testing.T) {
	f, a := newFake()
	f.createRes = pocketsdk.PocketCreateInvoiceResponse{}
	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o", Amount: 1})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "create_invoice" {
		t.Fatalf("err = %v, want create_invoice ProviderError", err)
	}
}

func TestTokenReusedUntilNearExpiry(t *testing.T) {
	f, a := newFake()
	in := core.CreateInvoiceInput{OrderID: "o", Amount: 1}

	for range 3 {
		if _, err := a.CreateInvoice(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	if f.logins != 1 {
		t.Fatalf("logins = %d after 3 calls on a fresh token, want 1", f.logins)
	}

	*f.clock = f.clock.Add(f.lifetime - 10*time.Second)
	if _, err := a.CreateInvoice(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if f.logins != 2 {
		t.Fatalf("logins = %d once the token is about to expire, want 2", f.logins)
	}
}

func TestTokenWithoutExpiryUsedOnce(t *testing.T) {
	f, a := newFake()
	f.lifetime = 0
	in := core.CreateInvoiceInput{OrderID: "o", Amount: 1}
	for range 2 {
		if _, err := a.CreateInvoice(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	if f.logins != 2 {
		t.Fatalf("logins = %d, want one per call", f.logins)
	}
}

func TestRejectedTokenRetriedOnce(t *testing.T) {
	f, a := newFake()
	f.accepts = func(tok string) bool { return tok == "tok-2" }

	if _, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o", Amount: 1}); err != nil {
		t.Fatal(err)
	}
	if f.logins != 2 || len(f.created) != 1 {
		t.Fatalf("logins = %d, invoices = %d; want 2 logins and 1 invoice", f.logins, len(f.created))
	}
}

func TestRejectedTokenGivesUpAfterOneRetry(t *testing.T) {
	f, a := newFake()
	f.accepts = func(string) bool { return false }

	_, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "4821"})
	if !errors.Is(err, pocketsdk.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "check_invoice" {
		t.Fatalf("err = %v, want check_invoice ProviderError", err)
	}
	if f.logins != 2 {
		t.Fatalf("logins = %d, want 2", f.logins)
	}
}

func TestLoginFailure(t *testing.T) {
	f, a := newFake()
	f.loginErr = errors.New("sso down")
	_, err := a.CreateInvoice(context.Background(), core.CreateInvoiceInput{OrderID: "o", Amount: 1})
	var pe *core.ProviderError
	if !errors.As(err, &pe) || pe.Op != "create_invoice" {
		t.Fatalf("err = %v, want create_invoice ProviderError", err)
	}
}

func TestCheckInvoice(t *testing.T) {
	f, a := newFake()
	f.detail = pocketsdk.PocketInvoiceDetailResponse{ID: 4821, State: "paid", Amount: 10000, OrderNumber: "order-1", InvoiceType: "ZERO"}

	st, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "4821", OrderID: "order-1"})
	if err != nil {
		t.Fatal(err)
	}
	if f.lookedUp != "invoice:4821" {
		t.Errorf("looked up %q, want by invoice id", f.lookedUp)
	}
	if st.Status != core.StatusPaid || st.PaidAmount != 10000 || st.InvoiceID != "4821" {
		t.Errorf("status = %+v", st)
	}
}

func TestCheckInvoiceByOrderNumber(t *testing.T) {
	f, a := newFake()
	f.detail = pocketsdk.PocketInvoiceDetailResponse{ID: 4821, State: "pending", Amount: 10000}

	st, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{OrderID: "order-1"})
	if err != nil {
		t.Fatal(err)
	}
	if f.lookedUp != "order:order-1" {
		t.Errorf("looked up %q, want by order number", f.lookedUp)
	}
	if st.Status != core.StatusPending || st.PaidAmount != 0 || st.InvoiceID != "4821" {
		t.Errorf("status = %+v", st)
	}
}

func TestCheckInvoiceInvalid(t *testing.T) {
	f, a := newFake()
	if _, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
	f.detail = pocketsdk.PocketInvoiceDetailResponse{}
	var pe *core.ProviderError
	if _, err := a.CheckInvoice(context.Background(), core.CheckInvoiceInput{InvoiceID: "1"}); !errors.As(err, &pe) {
		t.Fatalf("empty reply err = %v, want ProviderError", err)
	}
}

func TestInvoiceStatus(t *testing.T) {
	cases := map[string]core.Status{
		"paid":       core.StatusPaid,
		"pending":    core.StatusPending,
		"processing": core.StatusPending,
		"processed":  core.StatusPending,
		"cancelled":  core.StatusCancelled,
		"rejected":   core.StatusFailed,
		"unsuccess":  core.StatusFailed,
		"brand-new":  core.StatusPending,
	}
	for state, want := range cases {
		if got := invoiceStatus(state); got != want {
			t.Errorf("invoiceStatus(%q) = %q, want %q", state, got, want)
		}
	}
}
