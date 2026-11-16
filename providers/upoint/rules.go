package upoint

import "github.com/techpartners-asia/payment-all-in-one/core"

func (a *Adapter) InvoiceRules() core.InvoiceRules {
	return core.InvoiceRules{
		AnyOf: [][]core.Field{{core.FieldPhone, core.ExtraField("card_number")}},
		Extra: map[string]core.ExtraRule{
			"card_number":  {Doc: "UPoint card number; required when Phone is empty"},
			"total_amount": {Doc: "bill total (default Amount)", Number: true},
			"bonus_amount": {Doc: "part of the bill that earns points (default 0)", Number: true},
			"inter_number": {Doc: "Unitel contract number"},
		},
	}
}
