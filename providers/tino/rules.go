package tino

import "github.com/techpartners-asia/payment-all-in-one/core"

func (a *Adapter) InvoiceRules() core.InvoiceRules {
	return core.InvoiceRules{Extra: map[string]core.ExtraRule{
		"wallet_id": {Doc: "overrides Config.WalletID for this invoice"},
		"meta_data": {Doc: "free-form string stored on the invoice"},
	}}
}
