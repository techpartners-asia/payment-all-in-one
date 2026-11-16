package bonum

import (
	"fmt"
	"strings"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

func (a *Adapter) InvoiceRules() core.InvoiceRules {
	return core.InvoiceRules{
		Extra: map[string]core.ExtraRule{
			"flow":      {Doc: "qr (default) or checkout; checkout needs ReturnURL here or in Config", Values: []string{"qr", "checkout"}},
			"providers": {Doc: "checkout only: comma-separated list such as QPAY,E_COMMERCE"},
		},
		Check: func(in core.CreateInvoiceInput) error {
			if strings.EqualFold(in.Extra["flow"], "checkout") && in.ReturnURL == "" && a.cfg.ReturnURL == "" {
				return fmt.Errorf("%w: %s: ReturnURL is required for the checkout flow (or set Config.ReturnURL)", core.ErrInvalidInput, core.ProviderBonum)
			}
			return nil
		},
	}
}
