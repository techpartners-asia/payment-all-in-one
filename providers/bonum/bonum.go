// Package bonum adapts the Bonum payment gateway (github.com/techpartners-asia/bonum-go)
// to core.Gateway.
//
// CreateInvoice opens a QR invoice by default: the result carries the QPay-compatible QR
// string, a base64 PNG and bank deeplinks. With Extra["flow"] == "checkout" it opens a
// hosted checkout invoice instead and returns its page as PaymentURL. The SDK fetches and
// refreshes its own access token.
//
// Bonum reports payment outcomes only through its webhook, which is registered on the
// merchant portal rather than per invoice, so CreateInvoiceInput.CallbackURL is ignored.
// Verify webhooks with bonum.ParseWebhook from the SDK. Bonum forbids polling invoice
// status in production, so CheckInvoice returns core.ErrUnsupported.
//
// Extra keys read by CreateInvoice:
//   - "flow": "qr" (default) or "checkout".
//   - "providers": checkout only, comma-separated payment methods to offer, e.g.
//     "QPAY,E_COMMERCE". Omitted means every method enabled on the terminal.
//
// The checkout flow redirects the customer's browser to CreateInvoiceInput.ReturnURL,
// falling back to Config.ReturnURL; one of them is required for that flow.
//
// Optional operations: none. Bonum's only reversal (Cards.Reverse) applies to stored-card
// purchases, not to QR or checkout invoices.
package bonum

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	bonum "github.com/techpartners-asia/bonum-go"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

const defaultExpiry = 15 * time.Minute

type Config struct {
	AppSecret  string
	TerminalID string
	// Sandbox selects https://testapi.bonum.mn instead of https://apis.bonum.mn.
	Sandbox bool
	// BaseURL overrides the host chosen by Sandbox, e.g. for a proxy.
	BaseURL string
	// ReturnURL is where the checkout flow sends the customer's browser afterwards.
	ReturnURL string
}

type Adapter struct {
	cfg    Config
	client *bonum.Client
}

var _ core.Gateway = (*Adapter)(nil)

func New(cfg Config) (*Adapter, error) {
	if err := core.RequireConfig(core.ProviderBonum,
		"AppSecret", cfg.AppSecret,
		"TerminalID", cfg.TerminalID,
	); err != nil {
		return nil, err
	}
	env := bonum.Production
	if cfg.Sandbox {
		env = bonum.Sandbox
	}
	var opts []bonum.Option
	if cfg.BaseURL != "" {
		opts = append(opts, bonum.WithBaseURL(cfg.BaseURL))
	}
	return &Adapter{cfg: cfg, client: bonum.New(env, cfg.AppSecret, cfg.TerminalID, opts...)}, nil
}

func (a *Adapter) Provider() core.Provider { return core.ProviderBonum }

func (a *Adapter) CreateInvoice(ctx context.Context, in core.CreateInvoiceInput) (*core.Invoice, error) {
	expiresIn := int64(in.ExpiresIn / time.Second)
	if expiresIn <= 0 {
		expiresIn = int64(defaultExpiry / time.Second)
	}
	switch flow := in.Extra["flow"]; flow {
	case "", "qr":
		return a.createQR(ctx, in, expiresIn)
	case "checkout":
		return a.createCheckout(ctx, in, expiresIn)
	default:
		return nil, fmt.Errorf("%w: bonum flow %q, want qr or checkout", core.ErrInvalidInput, flow)
	}
}

func (a *Adapter) createQR(ctx context.Context, in core.CreateInvoiceInput, expiresIn int64) (*core.Invoice, error) {
	res, err := a.client.QR.Create(ctx, bonum.CreateQRInput{
		Amount:        in.Amount,
		TransactionID: in.OrderID,
		ExpiresIn:     expiresIn,
	})
	if err != nil {
		return nil, core.Wrap(core.ProviderBonum, "create_invoice", err)
	}
	if res.InvoiceID == "" || res.QrCode == "" {
		return nil, core.Wrap(core.ProviderBonum, "create_invoice", errEmptyInvoice)
	}
	links := make([]core.Deeplink, 0, len(res.Links))
	for _, l := range res.Links {
		links = append(links, core.Deeplink{Name: l.Name, Description: l.Description, Logo: l.Logo, Link: l.Link})
	}
	return &core.Invoice{
		Provider:  core.ProviderBonum,
		InvoiceID: res.InvoiceID,
		OrderID:   in.OrderID,
		Amount:    in.Amount,
		QRText:    res.QrCode,
		QRImage:   res.QrImage,
		Deeplinks: links,
		Status:    core.StatusPending,
		Raw:       res,
	}, nil
}

// The gateway client decodes leniently, so an HTML 200 from a proxy yields an
// empty result instead of an error.
var errEmptyInvoice = errors.New("response carried no invoice")

func (a *Adapter) createCheckout(ctx context.Context, in core.CreateInvoiceInput, expiresIn int64) (*core.Invoice, error) {
	returnURL := in.ReturnURL
	if returnURL == "" {
		returnURL = a.cfg.ReturnURL
	}
	if returnURL == "" {
		return nil, fmt.Errorf("%w: bonum checkout requires ReturnURL", core.ErrInvalidInput)
	}
	req := bonum.CreateInvoiceInput{
		Amount:        in.Amount,
		Callback:      returnURL,
		TransactionID: in.OrderID,
		ExpiresIn:     expiresIn,
	}
	if p := in.Extra["providers"]; p != "" {
		for name := range strings.SplitSeq(p, ",") {
			req.Providers = append(req.Providers, bonum.PaymentProvider(strings.TrimSpace(name)))
		}
	}
	res, err := a.client.Invoices.Create(ctx, req)
	if err != nil {
		return nil, core.Wrap(core.ProviderBonum, "create_invoice", err)
	}
	if res.ID == "" || res.FollowUpLink == "" {
		return nil, core.Wrap(core.ProviderBonum, "create_invoice", errEmptyInvoice)
	}
	return &core.Invoice{
		Provider:   core.ProviderBonum,
		InvoiceID:  res.ID,
		OrderID:    in.OrderID,
		Amount:     in.Amount,
		PaymentURL: res.FollowUpLink,
		Status:     core.StatusPending,
		Raw:        res,
	}, nil
}

func (a *Adapter) CheckInvoice(context.Context, core.CheckInvoiceInput) (*core.PaymentStatus, error) {
	return nil, core.Wrap(core.ProviderBonum, "check_invoice",
		fmt.Errorf("%w: bonum reports payments only by webhook; verify it with bonum.ParseWebhook", core.ErrUnsupported))
}
