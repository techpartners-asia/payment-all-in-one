package qpay

import "github.com/techpartners-asia/payment-all-in-one/core"

func (a *Adapter) InvoiceRules() core.InvoiceRules {
	r := core.InvoiceRules{WholeAmount: true, Extra: map[string]core.ExtraRule{
		"invoice_receiver_code": {Doc: "receiver code when CustomerID is empty (default \"terminal\")"},
		"sender_branch_code":    {Doc: "merchant branch code"},
		"note":                  {Doc: "invoice note"},
	}}
	if a.callbackURL == "" {
		r.Required = []core.Field{core.FieldCallbackURL}
	}
	return r
}
