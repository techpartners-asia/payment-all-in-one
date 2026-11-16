package ardpay

import "github.com/techpartners-asia/payment-all-in-one/core"

func (a *Adapter) InvoiceRules() core.InvoiceRules {
	return core.InvoiceRules{Extra: map[string]core.ExtraRule{
		"paid_limit": {Doc: "number passed as ArdPay paidLimit (default 0)", Number: true},
	}}
}
