// Package payment is a single entry point over every payment provider the
// techpartners-asia SDKs cover. Configure the providers you use, then route
// invoices by core.Provider:
//
//	client, err := payment.New(payment.Config{
//		QPay: &qpay.Config{...},
//		Bonum: &bonum.Config{...},
//	})
//	inv, err := client.CreateInvoice(ctx, payment.CreateInvoiceInput{
//		Provider: payment.ProviderQPay, OrderID: "order-1", Amount: 15000,
//	})
//	st, err := client.CheckInvoice(ctx, payment.CheckInvoiceInput{
//		Provider: inv.Provider, InvoiceID: inv.InvoiceID, OrderID: inv.OrderID,
//	})
package payment

import (
	"context"
	"fmt"
	"slices"

	"github.com/techpartners-asia/payment-all-in-one/core"
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

type (
	Provider           = core.Provider
	Status             = core.Status
	Gateway            = core.Gateway
	CreateInvoiceInput = core.CreateInvoiceInput
	Invoice            = core.Invoice
	Deeplink           = core.Deeplink
	CheckInvoiceInput  = core.CheckInvoiceInput
	PaymentStatus      = core.PaymentStatus
	CancelInvoiceInput = core.CancelInvoiceInput
	RefundInput        = core.RefundInput
	ProviderError      = core.ProviderError
	InvoiceRules       = core.InvoiceRules
	Field              = core.Field
)

const (
	ProviderTino       = core.ProviderTino
	ProviderQPay       = core.ProviderQPay
	ProviderGolomt     = core.ProviderGolomt
	ProviderSocialPay  = core.ProviderSocialPay
	ProviderMonPay     = core.ProviderMonPay
	ProviderPocket     = core.ProviderPocket
	ProviderBonum      = core.ProviderBonum
	ProviderHiPay      = core.ProviderHiPay
	ProviderDigiPay    = core.ProviderDigiPay
	ProviderArdPay     = core.ProviderArdPay
	ProviderMongolChat = core.ProviderMongolChat
	ProviderUPoint     = core.ProviderUPoint
	ProviderSimple     = core.ProviderSimple
	ProviderStorePay   = core.ProviderStorePay
	ProviderBalc       = core.ProviderBalc

	StatusPending   = core.StatusPending
	StatusPaid      = core.StatusPaid
	StatusFailed    = core.StatusFailed
	StatusCancelled = core.StatusCancelled
	StatusRefunded  = core.StatusRefunded
)

const (
	FieldOrderID     = core.FieldOrderID
	FieldAmount      = core.FieldAmount
	FieldDescription = core.FieldDescription
	FieldCallbackURL = core.FieldCallbackURL
	FieldReturnURL   = core.FieldReturnURL
	FieldPhone       = core.FieldPhone
	FieldCustomerID  = core.FieldCustomerID
)

func ExtraField(key string) Field { return core.ExtraField(key) }

var (
	ErrNotConfigured = core.ErrNotConfigured
	ErrUnsupported   = core.ErrUnsupported
	ErrInvalidInput  = core.ErrInvalidInput
	ErrInvalidConfig = core.ErrInvalidConfig
)

// Config enables a provider by setting its pointer. Nil providers stay disabled.
type Config struct {
	Tino       *tino.Config
	QPay       *qpay.Config
	Golomt     *golomt.Config
	SocialPay  *socialpay.Config
	MonPay     *monpay.Config
	Pocket     *pocket.Config
	Bonum      *bonum.Config
	HiPay      *hipay.Config
	DigiPay    *digipay.Config
	ArdPay     *ardpay.Config
	MongolChat *mongolchat.Config
	UPoint     *upoint.Config
	Simple     *simple.Config
	StorePay   *storepay.Config
	Balc       *balc.Config
}

type Client struct {
	gateways map[Provider]Gateway
}

// New builds an adapter for every provider set in cfg. Any invalid provider
// config fails the whole call so misconfiguration surfaces at startup.
func New(cfg Config) (*Client, error) {
	c := &Client{gateways: map[Provider]Gateway{}}
	builders := []func() (Gateway, error){
		build(cfg.Tino, tino.New),
		build(cfg.QPay, qpay.New),
		build(cfg.Golomt, golomt.New),
		build(cfg.SocialPay, socialpay.New),
		build(cfg.MonPay, monpay.New),
		build(cfg.Pocket, pocket.New),
		build(cfg.Bonum, bonum.New),
		build(cfg.HiPay, hipay.New),
		build(cfg.DigiPay, digipay.New),
		build(cfg.ArdPay, ardpay.New),
		build(cfg.MongolChat, mongolchat.New),
		build(cfg.UPoint, upoint.New),
		build(cfg.Simple, simple.New),
		build(cfg.StorePay, storepay.New),
		build(cfg.Balc, balc.New),
	}
	for _, b := range builders {
		gw, err := b()
		if err != nil {
			return nil, err
		}
		if gw != nil {
			c.Register(gw)
		}
	}
	return c, nil
}

func build[C any, A Gateway](cfg *C, newFn func(C) (A, error)) func() (Gateway, error) {
	return func() (Gateway, error) {
		if cfg == nil {
			return nil, nil
		}
		return newFn(*cfg)
	}
}

// Register adds or replaces the gateway for gw.Provider(). Use it for custom
// providers or test doubles.
func (c *Client) Register(gw Gateway) {
	if c.gateways == nil {
		c.gateways = map[Provider]Gateway{}
	}
	c.gateways[gw.Provider()] = gw
}

// Providers lists the configured providers in a stable order.
func (c *Client) Providers() []Provider {
	ps := make([]Provider, 0, len(c.gateways))
	for p := range c.gateways {
		ps = append(ps, p)
	}
	slices.Sort(ps)
	return ps
}

func (c *Client) Gateway(p Provider) (Gateway, error) {
	gw, ok := c.gateways[p]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotConfigured, p)
	}
	return gw, nil
}

// InvoiceRules reports what CreateInvoice requires and accepts for p, so a
// checkout form can ask for the right fields up front.
func (c *Client) InvoiceRules(p Provider) (InvoiceRules, error) {
	gw, err := c.Gateway(p)
	if err != nil {
		return InvoiceRules{}, err
	}
	return gw.InvoiceRules(), nil
}

// ValidateInvoice runs every CreateInvoice input check without calling the
// provider. The error joins all problems found, each wrapping ErrInvalidInput.
func (c *Client) ValidateInvoice(in CreateInvoiceInput) error {
	gw, err := c.Gateway(in.Provider)
	if err != nil {
		return err
	}
	return gw.InvoiceRules().Validate(in.Provider, in)
}

func (c *Client) CreateInvoice(ctx context.Context, in CreateInvoiceInput) (*Invoice, error) {
	if err := c.ValidateInvoice(in); err != nil {
		return nil, err
	}
	gw, err := c.Gateway(in.Provider)
	if err != nil {
		return nil, err
	}
	return gw.CreateInvoice(ctx, in)
}

func (c *Client) CheckInvoice(ctx context.Context, in CheckInvoiceInput) (*PaymentStatus, error) {
	if in.InvoiceID == "" && in.OrderID == "" {
		return nil, fmt.Errorf("%w: invoice id or order id is required", ErrInvalidInput)
	}
	gw, err := c.Gateway(in.Provider)
	if err != nil {
		return nil, err
	}
	st, err := gw.CheckInvoice(ctx, in)
	if err != nil {
		return nil, err
	}
	return guardUnderpayment(in, st), nil
}

// guardUnderpayment downgrades a paid status to pending when the provider
// reports less than the caller expected, so a cheaper invoice's id cannot
// settle a more expensive order. Providers that report no paid amount cannot
// be checked here; see the README.
func guardUnderpayment(in CheckInvoiceInput, st *PaymentStatus) *PaymentStatus {
	if st == nil || st.Status != StatusPaid || in.Amount <= 0 || st.PaidAmount <= 0 {
		return st
	}
	if st.PaidAmount+0.005 < in.Amount {
		st.Status = StatusPending
	}
	return st
}

func (c *Client) CancelInvoice(ctx context.Context, in CancelInvoiceInput) error {
	gw, err := c.Gateway(in.Provider)
	if err != nil {
		return err
	}
	canceler, ok := gw.(core.Canceler)
	if !ok {
		return fmt.Errorf("%w: %s cancel", ErrUnsupported, in.Provider)
	}
	return canceler.CancelInvoice(ctx, in)
}

func (c *Client) Refund(ctx context.Context, in RefundInput) error {
	if in.Amount < 0 {
		return fmt.Errorf("%w: refund amount must not be negative", ErrInvalidInput)
	}
	gw, err := c.Gateway(in.Provider)
	if err != nil {
		return err
	}
	refunder, ok := gw.(core.Refunder)
	if !ok {
		return fmt.Errorf("%w: %s refund", ErrUnsupported, in.Provider)
	}
	return refunder.Refund(ctx, in)
}
