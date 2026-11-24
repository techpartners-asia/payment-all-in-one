package storepay

import "github.com/techpartners-asia/payment-all-in-one/core"

func (a *Adapter) InvoiceRules() core.InvoiceRules {
	return core.InvoiceRules{Required: []core.Field{core.FieldPhone}}
}
