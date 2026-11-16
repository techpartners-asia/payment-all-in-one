package socialpay

import "github.com/techpartners-asia/payment-all-in-one/core"

// Phone is optional: when set the invoice goes to that SocialPay user,
// otherwise a QR invoice is created.
func (a *Adapter) InvoiceRules() core.InvoiceRules { return core.InvoiceRules{} }
