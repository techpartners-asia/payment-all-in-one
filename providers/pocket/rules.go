package pocket

import "github.com/techpartners-asia/payment-all-in-one/core"

func (a *Adapter) InvoiceRules() core.InvoiceRules {
	return core.InvoiceRules{Extra: map[string]core.ExtraRule{
		"invoice_type": {Doc: "ZERO (default) or PURCHASE", Values: []string{"ZERO", "PURCHASE"}},
		"channel":      {Doc: "Pocket channel (default ecommerce)"},
	}}
}
