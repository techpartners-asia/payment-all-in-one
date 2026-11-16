package payment_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"testing"

	payment "github.com/techpartners-asia/payment-all-in-one"
	"github.com/techpartners-asia/payment-all-in-one/providers/ardpay"
	"github.com/techpartners-asia/payment-all-in-one/providers/balc"
	"github.com/techpartners-asia/payment-all-in-one/providers/bonum"
	"github.com/techpartners-asia/payment-all-in-one/providers/digipay"
	"github.com/techpartners-asia/payment-all-in-one/providers/golomt"
	"github.com/techpartners-asia/payment-all-in-one/providers/hipay"
	"github.com/techpartners-asia/payment-all-in-one/providers/mongolchat"
	"github.com/techpartners-asia/payment-all-in-one/providers/monpay"
	"github.com/techpartners-asia/payment-all-in-one/providers/pocket"
	"github.com/techpartners-asia/payment-all-in-one/providers/qpay"
	"github.com/techpartners-asia/payment-all-in-one/providers/simple"
	"github.com/techpartners-asia/payment-all-in-one/providers/socialpay"
	"github.com/techpartners-asia/payment-all-in-one/providers/storepay"
	"github.com/techpartners-asia/payment-all-in-one/providers/tino"
	"github.com/techpartners-asia/payment-all-in-one/providers/upoint"
)

// deadURL refuses connections, so any adapter call that gets past input
// validation fails as a provider error without touching the network.
func deadURL(t *testing.T) string {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}

// allProviders configures every provider against dead. QPay and Golomt get
// no default callback so their conditional CallbackURL rule is exercised.
func allProviders(t *testing.T) *payment.Client {
	t.Helper()
	dead := deadURL(t)
	c, err := payment.New(payment.Config{
		Tino:       &tino.Config{AuthURL: dead, BaseURL: dead, Username: "u", Password: "p", WalletID: "w"},
		QPay:       &qpay.Config{Username: "u", Password: "p", Endpoint: dead, InvoiceCode: "INV"},
		Golomt:     &golomt.Config{BaseURL: dead, Secret: "s", BearerToken: "b"},
		SocialPay:  &socialpay.Config{Terminal: "t", Secret: "s", Endpoint: dead},
		MonPay:     &monpay.Config{Endpoint: dead, Username: "u", AccountID: "a"},
		Pocket:     &pocket.Config{ClientID: "c", ClientSecret: "s", TerminalID: 1},
		Bonum:      &bonum.Config{AppSecret: "s", TerminalID: "t", BaseURL: dead, ReturnURL: "https://shop.test/return"},
		HiPay:      &hipay.Config{Endpoint: dead, Token: "t", EntityID: "e"},
		DigiPay:    &digipay.Config{BaseURL: dead, ClientID: "c", ClientSecret: "s", MerchantID: "m", MerchantName: "n", TerminalID: "t"},
		ArdPay:     &ardpay.Config{BaseURL: dead, MerchantID: "m", PosNo: "p", APIKey: "k"},
		MongolChat: &mongolchat.Config{Endpoint: dead, APIKey: "k", WorkerKey: "w"},
		UPoint:     &upoint.Config{Endpoint: dead, Token: "t", TerminalID: "t"},
		Simple:     &simple.Config{Username: "u", Password: "p", BaseURL: dead},
		StorePay:   &storepay.Config{AppUsername: "a", AppPassword: "p", Username: "u", Password: "p", AuthURL: dead, BaseURL: dead, StoreID: "s"},
		Balc:       &balc.Config{Endpoint: dead, Token: "t"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(c.Providers()); got != 15 {
		t.Fatalf("configured %d providers, want 15", got)
	}
	return c
}

var sampleValues = map[payment.Field]string{
	payment.FieldDescription: "Order description",
	payment.FieldCallbackURL: "https://shop.test/callback",
	payment.FieldReturnURL:   "https://shop.test/return",
	payment.FieldPhone:       "99112233",
	payment.FieldCustomerID:  "42",
}

func set(in *payment.CreateInvoiceInput, f payment.Field, v string) {
	switch f {
	case payment.FieldDescription:
		in.Description = v
	case payment.FieldCallbackURL:
		in.CallbackURL = v
	case payment.FieldReturnURL:
		in.ReturnURL = v
	case payment.FieldPhone:
		in.Phone = v
	case payment.FieldCustomerID:
		in.CustomerID = v
	default:
		key, ok := extraKey(f)
		if !ok {
			panic("unknown field " + f)
		}
		if in.Extra == nil {
			in.Extra = map[string]string{}
		}
		in.Extra[key] = v
	}
}

func extraKey(f payment.Field) (string, bool) {
	const prefix = "Extra."
	if len(f) > len(prefix) && string(f[:len(prefix)]) == prefix {
		return string(f[len(prefix):]), true
	}
	return "", false
}

func sampleFor(rules payment.InvoiceRules, f payment.Field) string {
	if key, ok := extraKey(f); ok {
		r := rules.Extra[key]
		switch {
		case len(r.Values) > 0:
			return r.Values[0]
		case r.Number:
			return "1"
		}
		return "sample"
	}
	return sampleValues[f]
}

// minimalInput builds the smallest input the declared rules accept.
func minimalInput(p payment.Provider, rules payment.InvoiceRules) payment.CreateInvoiceInput {
	in := payment.CreateInvoiceInput{Provider: p, OrderID: "order-1", Amount: 1000}
	for _, f := range rules.Required {
		set(&in, f, sampleFor(rules, f))
	}
	for _, group := range rules.AnyOf {
		set(&in, group[0], sampleFor(rules, group[0]))
	}
	return in
}

// reachesProvider reports whether CreateInvoice got past every input check.
// Pocket's hosts are hardcoded, so it is checked against its rules only.
func reachesProvider(t *testing.T, c *payment.Client, in payment.CreateInvoiceInput) {
	t.Helper()
	if in.Provider == payment.ProviderPocket {
		if err := c.ValidateInvoice(in); err != nil {
			t.Fatalf("rules reject %+v: %v", in, err)
		}
		return
	}
	_, err := c.CreateInvoice(context.Background(), in)
	if err == nil {
		t.Fatalf("create against a dead server succeeded for %+v", in)
	}
	if errors.Is(err, payment.ErrInvalidInput) {
		t.Fatalf("input accepted by the declared rules was rejected by the adapter: %v\ninput: %+v", err, in)
	}
}

func TestRulesMatchAdapterBehavior(t *testing.T) {
	c := allProviders(t)
	for _, p := range c.Providers() {
		t.Run(string(p), func(t *testing.T) {
			rules, err := c.InvoiceRules(p)
			if err != nil {
				t.Fatal(err)
			}
			base := minimalInput(p, rules)

			if err := c.ValidateInvoice(base); err != nil {
				t.Fatalf("minimal input fails its own rules: %v", err)
			}
			reachesProvider(t, c, base)

			for _, f := range rules.Required {
				in := minimalInput(p, rules)
				set(&in, f, "")
				if err := c.ValidateInvoice(in); !errors.Is(err, payment.ErrInvalidInput) {
					t.Errorf("missing required %s: err = %v, want ErrInvalidInput", f, err)
				}
			}
			for _, group := range rules.AnyOf {
				in := minimalInput(p, rules)
				for _, f := range group {
					set(&in, f, "")
				}
				if err := c.ValidateInvoice(in); !errors.Is(err, payment.ErrInvalidInput) {
					t.Errorf("none of %v: err = %v, want ErrInvalidInput", group, err)
				}
				for _, f := range group[1:] {
					alt := minimalInput(p, rules)
					set(&alt, group[0], "")
					set(&alt, f, sampleFor(rules, f))
					reachesProvider(t, c, alt)
				}
			}

			for _, key := range slices.Sorted(maps.Keys(rules.Extra)) {
				r := rules.Extra[key]
				values := []string{"sample"}
				switch {
				case len(r.Values) > 0:
					values = r.Values
				case r.Number:
					values = []string{"0", "12.5"}
				}
				for _, v := range values {
					in := minimalInput(p, rules)
					set(&in, payment.ExtraField(key), v)
					reachesProvider(t, c, in)
				}
				if len(r.Values) > 0 || r.Number {
					in := minimalInput(p, rules)
					set(&in, payment.ExtraField(key), "not-a-valid-value")
					if err := c.ValidateInvoice(in); !errors.Is(err, payment.ErrInvalidInput) {
						t.Errorf("Extra[%q] invalid value: err = %v, want ErrInvalidInput", key, err)
					}
				}
			}

			typo := minimalInput(p, rules)
			set(&typo, payment.ExtraField("definitely_not_a_key"), "x")
			if err := c.ValidateInvoice(typo); !errors.Is(err, payment.ErrInvalidInput) {
				t.Errorf("unknown Extra key: err = %v, want ErrInvalidInput", err)
			}

			fractional := minimalInput(p, rules)
			fractional.Amount = 1000.5
			if rules.WholeAmount {
				if err := c.ValidateInvoice(fractional); !errors.Is(err, payment.ErrInvalidInput) {
					t.Errorf("fractional amount with WholeAmount: err = %v, want ErrInvalidInput", err)
				}
			} else {
				reachesProvider(t, c, fractional)
			}
		})
	}
}

func TestBonumCheckoutReturnURLIsCheckedUpFront(t *testing.T) {
	c, err := payment.New(payment.Config{Bonum: &bonum.Config{AppSecret: "s", TerminalID: "t", BaseURL: deadURL(t)}})
	if err != nil {
		t.Fatal(err)
	}
	in := payment.CreateInvoiceInput{Provider: payment.ProviderBonum, OrderID: "o", Amount: 1000, Extra: map[string]string{"flow": "Checkout"}}
	if err := c.ValidateInvoice(in); !errors.Is(err, payment.ErrInvalidInput) {
		t.Fatalf("checkout without ReturnURL: err = %v, want ErrInvalidInput", err)
	}
	in.ReturnURL = "https://shop.test/return"
	if err := c.ValidateInvoice(in); err != nil {
		t.Fatalf("checkout with ReturnURL: %v", err)
	}
	in.Extra = nil
	in.ReturnURL = ""
	if err := c.ValidateInvoice(in); err != nil {
		t.Fatalf("qr flow needs no ReturnURL: %v", err)
	}
}

func TestValidateInvoiceReportsEveryProblem(t *testing.T) {
	c := allProviders(t)
	err := c.ValidateInvoice(payment.CreateInvoiceInput{
		Provider: payment.ProviderStorePay,
		Amount:   -5,
		Extra:    map[string]string{"phone": "99112233"},
	})
	for _, want := range []string{"OrderID is required", "Amount must be positive", "Phone is required", `unknown Extra key "phone"`} {
		if err == nil || !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(err.Error()) {
			t.Errorf("error %q does not report %q", err, want)
		}
	}
}

// TestDeclaredExtraKeysMatchSource fails when an adapter starts reading an
// Extra key without declaring it, or declares one it never reads.
func TestDeclaredExtraKeysMatchSource(t *testing.T) {
	c := allProviders(t)
	read := regexp.MustCompile(`(?:Extra|extra)\["([a-z_]+)"\]|(?:Extra|extra), "([a-z_]+)"`)
	for _, p := range c.Providers() {
		src, err := os.ReadFile("providers/" + string(p) + "/" + string(p) + ".go")
		if err != nil {
			t.Fatal(err)
		}
		used := map[string]bool{}
		for _, m := range read.FindAllStringSubmatch(string(src), -1) {
			used[m[1]+m[2]] = true
		}
		rules, _ := c.InvoiceRules(p)
		for key := range used {
			if _, ok := rules.Extra[key]; !ok {
				t.Errorf("%s reads Extra[%q] but does not declare it", p, key)
			}
		}
		for key := range rules.Extra {
			if !used[key] {
				t.Errorf("%s declares Extra[%q] but never reads it", p, key)
			}
		}
	}
}
