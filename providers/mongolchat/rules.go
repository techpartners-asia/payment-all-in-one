package mongolchat

import "github.com/techpartners-asia/payment-all-in-one/core"

func (a *Adapter) InvoiceRules() core.InvoiceRules {
	return core.InvoiceRules{WholeAmount: true, Extra: map[string]core.ExtraRule{
		"branch_id":   {Doc: "overrides Config.BranchID"},
		"sub_title":   {Doc: "QR sub title"},
		"noat":        {Doc: "VAT amount, passed through"},
		"nhat":        {Doc: "city tax amount, passed through"},
		"ttd":         {Doc: "taxpayer number, passed through"},
		"expire_time": {Doc: "passed through verbatim in MongolChat's format"},
	}}
}
