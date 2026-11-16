package payment_test

import (
	"context"
	"errors"
	"testing"

	payment "github.com/techpartners-asia/payment-all-in-one"
	"github.com/techpartners-asia/payment-all-in-one/providers/qpay"
)

type fakeGateway struct {
	provider payment.Provider
	created  []payment.CreateInvoiceInput
}

func (f *fakeGateway) Provider() payment.Provider { return f.provider }

func (f *fakeGateway) CreateInvoice(_ context.Context, in payment.CreateInvoiceInput) (*payment.Invoice, error) {
	f.created = append(f.created, in)
	return &payment.Invoice{Provider: f.provider, InvoiceID: "inv-" + in.OrderID, OrderID: in.OrderID, Amount: in.Amount, Status: payment.StatusPending}, nil
}

func (f *fakeGateway) CheckInvoice(_ context.Context, in payment.CheckInvoiceInput) (*payment.PaymentStatus, error) {
	return &payment.PaymentStatus{Provider: f.provider, InvoiceID: in.InvoiceID, Status: payment.StatusPaid}, nil
}

type refundingGateway struct {
	fakeGateway
	refunds []payment.RefundInput
}

func (r *refundingGateway) Refund(_ context.Context, in payment.RefundInput) error {
	r.refunds = append(r.refunds, in)
	return nil
}

func TestNewWithEmptyConfigHasNoProviders(t *testing.T) {
	c, err := payment.New(payment.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Providers(); len(got) != 0 {
		t.Fatalf("providers = %v, want none", got)
	}
	_, err = c.CreateInvoice(context.Background(), payment.CreateInvoiceInput{Provider: payment.ProviderQPay, OrderID: "o1", Amount: 100})
	if !errors.Is(err, payment.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestNewRejectsIncompleteProviderConfig(t *testing.T) {
	_, err := payment.New(payment.Config{QPay: &qpay.Config{}})
	if !errors.Is(err, payment.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestClientRoutesToRegisteredGateway(t *testing.T) {
	c, _ := payment.New(payment.Config{})
	qpayGW := &fakeGateway{provider: payment.ProviderQPay}
	bonumGW := &fakeGateway{provider: payment.ProviderBonum}
	c.Register(qpayGW)
	c.Register(bonumGW)

	inv, err := c.CreateInvoice(context.Background(), payment.CreateInvoiceInput{Provider: payment.ProviderBonum, OrderID: "o1", Amount: 5000})
	if err != nil {
		t.Fatalf("CreateInvoice: %v", err)
	}
	if inv.Provider != payment.ProviderBonum || len(bonumGW.created) != 1 || len(qpayGW.created) != 0 {
		t.Fatalf("routed to wrong gateway: inv=%+v bonum=%d qpay=%d", inv, len(bonumGW.created), len(qpayGW.created))
	}

	st, err := c.CheckInvoice(context.Background(), payment.CheckInvoiceInput{Provider: payment.ProviderBonum, InvoiceID: inv.InvoiceID})
	if err != nil || st.Status != payment.StatusPaid {
		t.Fatalf("CheckInvoice = %+v, %v", st, err)
	}
	if got := c.Providers(); len(got) != 2 || got[0] != payment.ProviderBonum || got[1] != payment.ProviderQPay {
		t.Fatalf("providers = %v", got)
	}
}

func TestClientValidatesInput(t *testing.T) {
	c, _ := payment.New(payment.Config{})
	c.Register(&fakeGateway{provider: payment.ProviderQPay})
	ctx := context.Background()

	cases := map[string]error{
		"missing order": func() error {
			_, err := c.CreateInvoice(ctx, payment.CreateInvoiceInput{Provider: payment.ProviderQPay, Amount: 1})
			return err
		}(),
		"zero amount": func() error {
			_, err := c.CreateInvoice(ctx, payment.CreateInvoiceInput{Provider: payment.ProviderQPay, OrderID: "o"})
			return err
		}(),
		"check without ids": func() error {
			_, err := c.CheckInvoice(ctx, payment.CheckInvoiceInput{Provider: payment.ProviderQPay})
			return err
		}(),
	}
	for name, err := range cases {
		if !errors.Is(err, payment.ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
}

func TestOptionalOperations(t *testing.T) {
	c, _ := payment.New(payment.Config{})
	c.Register(&fakeGateway{provider: payment.ProviderQPay})
	rg := &refundingGateway{fakeGateway: fakeGateway{provider: payment.ProviderBonum}}
	c.Register(rg)
	ctx := context.Background()

	if err := c.Refund(ctx, payment.RefundInput{Provider: payment.ProviderQPay, InvoiceID: "x"}); !errors.Is(err, payment.ErrUnsupported) {
		t.Fatalf("refund on non-refunder: err = %v, want ErrUnsupported", err)
	}
	if err := c.CancelInvoice(ctx, payment.CancelInvoiceInput{Provider: payment.ProviderQPay, InvoiceID: "x"}); !errors.Is(err, payment.ErrUnsupported) {
		t.Fatalf("cancel on non-canceler: err = %v, want ErrUnsupported", err)
	}
	if err := c.Refund(ctx, payment.RefundInput{Provider: payment.ProviderBonum, InvoiceID: "x", Amount: 10}); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if len(rg.refunds) != 1 || rg.refunds[0].Amount != 10 {
		t.Fatalf("refunds = %+v", rg.refunds)
	}
}

type amountGateway struct {
	fakeGateway
	paid float64
}

func (a *amountGateway) CheckInvoice(_ context.Context, in payment.CheckInvoiceInput) (*payment.PaymentStatus, error) {
	return &payment.PaymentStatus{Provider: a.provider, InvoiceID: in.InvoiceID, Status: payment.StatusPaid, PaidAmount: a.paid}, nil
}

func TestCheckInvoiceRejectsUnderpayment(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		paid     float64
		expected float64
		want     payment.Status
	}{
		{"cheaper invoice", 100, 1_000_000, payment.StatusPending},
		{"exact", 1500, 1500, payment.StatusPaid},
		{"no expectation", 100, 0, payment.StatusPaid},
		{"provider reports no amount", 0, 1500, payment.StatusPaid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c payment.Client
			c.Register(&amountGateway{fakeGateway: fakeGateway{provider: payment.ProviderHiPay}, paid: tc.paid})
			st, err := c.CheckInvoice(ctx, payment.CheckInvoiceInput{Provider: payment.ProviderHiPay, InvoiceID: "i", Amount: tc.expected})
			if err != nil {
				t.Fatal(err)
			}
			if st.Status != tc.want {
				t.Fatalf("status = %s, want %s", st.Status, tc.want)
			}
		})
	}
}
