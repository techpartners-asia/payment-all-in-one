package golomt

import "github.com/techpartners-asia/payment-all-in-one/core"

func (a *Adapter) InvoiceRules() core.InvoiceRules {
	r := core.InvoiceRules{Extra: map[string]core.ExtraRule{
		"return_type":     {Doc: "GET (default), POST or MOBILE", Values: []string{"GET", "POST", "MOBILE"}},
		"lang":            {Doc: "MN (default) or EN", Values: []string{"MN", "EN"}},
		"payment_method":  {Doc: "payment (default, card) or socialpay", Values: []string{"payment", "socialpay"}},
		"get_token":       {Doc: "true to tokenize the card", Values: []string{"true", "false"}},
		"social_deeplink": {Doc: "true to return a SocialPay deeplink", Values: []string{"true", "false"}},
	}}
	if a.callbackURL == "" {
		r.Required = []core.Field{core.FieldCallbackURL}
	}
	return r
}
