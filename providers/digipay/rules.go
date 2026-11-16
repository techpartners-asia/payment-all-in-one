package digipay

import "github.com/techpartners-asia/payment-all-in-one/core"

func (a *Adapter) InvoiceRules() core.InvoiceRules {
	return core.InvoiceRules{
		Required: []core.Field{core.ExtraField("bearer_token")},
		Extra: map[string]core.ExtraRule{
			"bearer_token": {Doc: "the DigiPay superapp user's token (from ExchangeAuthorizationCode)"},
		},
	}
}
