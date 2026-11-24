// Package upoint adapts the UPoint loyalty API
// (github.com/techpartners-asia/upoint-go) to core.Gateway.
//
// UPoint is a points program, not an invoicing gateway. The adapter maps
// "pay with points" onto it:
//
//   - CreateInvoice calls process_transaction, which spends in.Amount points
//     from the customer's card against bill number in.OrderID. It settles
//     synchronously, so the returned invoice is already StatusPaid and
//     InvoiceID is the bill number. There is no QR, URL, or callback.
//   - CheckInvoice calls check_transaction by bill number (InvoiceID).
//     TransactionID is the UPoint receipt_id.
//   - Refund with Amount 0 calls cancel_transaction by bill number, reversing
//     the whole transaction. Refund with Amount > 0 calls return_transaction
//     for that many spent points and needs TransactionID (the receipt_id).
//
// UPoint signals business errors with a non-zero "result" in a 200 response;
// the adapter treats result 0 as success.
//
// The SDK takes no context and uses http.DefaultClient with no timeout, so
// ctx is ignored and a hung UPoint host blocks the call.
//
// Extra keys (CreateInvoice):
//   - card_number: UPoint card number. Required unless in.Phone is set.
//   - total_amount: bill total in MNT; defaults to in.Amount.
//   - bonus_amount: part of the bill that earns points; defaults to 0.
//   - inter_number: Unitel contract number, optional.
//
// in.Phone is sent as the customer mobile. Optional operations: core.Refunder.
// core.Canceler is not implemented because a points transaction is never in
// an unpaid state that could be voided.
package upoint

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	sdk "github.com/techpartners-asia/upoint-go"

	"github.com/techpartners-asia/payment-all-in-one/core"
)

type Config struct {
	// Endpoint is the UPoint host; the SDK appends /transaction/thirdparty/....
	Endpoint string
	// Token is sent as "Authorization: Token <Token>".
	Token      string
	TerminalID string
}

type Adapter struct {
	client sdk.Upoint
}

var (
	_ core.Gateway  = (*Adapter)(nil)
	_ core.Refunder = (*Adapter)(nil)
)

const billDateLayout = "2006/01/02 15:04:05"

func New(cfg Config) (*Adapter, error) {
	if err := core.RequireConfig(core.ProviderUPoint,
		"Endpoint", cfg.Endpoint,
		"Token", cfg.Token,
		"TerminalID", cfg.TerminalID,
	); err != nil {
		return nil, err
	}
	return &Adapter{client: sdk.New(cfg.Endpoint, cfg.Token, cfg.TerminalID)}, nil
}

func (a *Adapter) Provider() core.Provider { return core.ProviderUPoint }

func (a *Adapter) CreateInvoice(_ context.Context, in core.CreateInvoiceInput) (*core.Invoice, error) {
	if err := a.InvoiceRules().Validate(a.Provider(), in); err != nil {
		return nil, err
	}
	card := in.Extra["card_number"]
	if card == "" && in.Phone == "" {
		return nil, fmt.Errorf("%w: upoint requires Extra[\"card_number\"] or Phone", core.ErrInvalidInput)
	}
	if in.OrderID == "" {
		return nil, fmt.Errorf("%w: OrderID is required as the UPoint bill number", core.ErrInvalidInput)
	}
	if in.Amount <= 0 {
		return nil, fmt.Errorf("%w: amount must be positive", core.ErrInvalidInput)
	}
	total, err := extraAmount(in.Extra, "total_amount", in.Amount)
	if err != nil {
		return nil, err
	}
	bonus, err := extraAmount(in.Extra, "bonus_amount", 0)
	if err != nil {
		return nil, err
	}

	// UPoint marks bank, manufacturer and items as required but allows them
	// empty; nil slices would marshal as null.
	res, err := call("create_invoice", func() (sdk.UpointTransactionResponse, error) {
		return a.client.ProcessTransaction(sdk.UpointTransactionRequest{
			CardNumber:   card,
			InterNumber:  in.Extra["inter_number"],
			Mobile:       in.Phone,
			Date:         time.Now().Format(billDateLayout),
			BillNumber:   in.OrderID,
			SpendAmount:  in.Amount,
			BonusAmount:  bonus,
			TotalAmount:  total,
			Bank:         []sdk.UpointBankRequest{},
			Manufacturer: []sdk.UpointManufacturerRequest{},
			Items:        []sdk.UpointItemRequest{},
		})
	})
	if err != nil {
		return nil, err
	}
	if res.Result != 0 {
		return nil, resultError("create_invoice", res.Result, res.Message)
	}
	// The SDK drops JSON decode errors, so an error-shaped or empty 200 decodes
	// to Result 0. Only a receipt proves the points were spent.
	if res.ReceiptID == "" {
		return nil, core.Wrap(core.ProviderUPoint, "create_invoice", errNoReceipt)
	}
	return &core.Invoice{
		Provider:  core.ProviderUPoint,
		InvoiceID: in.OrderID,
		OrderID:   in.OrderID,
		Amount:    in.Amount,
		Status:    core.StatusPaid,
		Raw:       res,
	}, nil
}

func (a *Adapter) CheckInvoice(_ context.Context, in core.CheckInvoiceInput) (*core.PaymentStatus, error) {
	if in.InvoiceID == "" {
		return nil, fmt.Errorf("%w: InvoiceID (the UPoint bill number) is required", core.ErrInvalidInput)
	}
	res, err := call("check_invoice", func() (sdk.UpointCheckTransactionResponse, error) {
		return a.client.CheckTransaction(sdk.UpointCheckTransactionRequest{BillNumber: in.InvoiceID})
	})
	if err != nil {
		return nil, err
	}
	ps := &core.PaymentStatus{
		Provider:  core.ProviderUPoint,
		InvoiceID: in.InvoiceID,
		Status:    mapStatus(res),
		Raw:       res,
	}
	if ps.Status == core.StatusPaid {
		ps.PaidAmount = res.SpendPoint
		ps.TransactionID = res.ReceiptID
	}
	return ps, nil
}

func (a *Adapter) Refund(_ context.Context, in core.RefundInput) error {
	if in.Amount < 0 {
		return fmt.Errorf("%w: refund amount must not be negative", core.ErrInvalidInput)
	}
	if in.Amount == 0 {
		bill := in.InvoiceID
		if bill == "" {
			bill = in.OrderID
		}
		if bill == "" {
			return fmt.Errorf("%w: full refund needs InvoiceID or OrderID (the UPoint bill number)", core.ErrInvalidInput)
		}
		res, err := call("refund", func() (sdk.UpointCancelTransactionResponse, error) {
			return a.client.CancelTransaction(sdk.UpointCancelTransactionRequest{BillNumber: bill})
		})
		if err != nil {
			return err
		}
		if res.Result != 0 {
			return resultError("refund", float64(res.Result), res.Message)
		}
		if res.ReturnReceiptID == 0 {
			return core.Wrap(core.ProviderUPoint, "refund", errNoReceipt)
		}
		return nil
	}

	if in.TransactionID == "" {
		return fmt.Errorf("%w: partial refund needs TransactionID (the UPoint receipt_id)", core.ErrInvalidInput)
	}
	res, err := call("refund", func() (sdk.UpointReturnTransactionResponse, error) {
		return a.client.ReturnTransaction(sdk.UpointReturnTransactionRequest{
			ReceiptID:         in.TransactionID,
			RefundSpendAmount: in.Amount,
			Bank:              []sdk.UpointBankRequest{},
			Manufacturer:      []sdk.UpointManufacturerRequest{},
			Items:             []sdk.UpointItemRequest{},
		})
	})
	if err != nil {
		return err
	}
	if res.Result != 0 {
		return resultError("refund", float64(res.Result), res.Message)
	}
	if res.ReturnReceiptID == 0 {
		return core.Wrap(core.ProviderUPoint, "refund", errNoReceipt)
	}
	return nil
}

var errNoReceipt = errors.New("response carried no receipt id")

// mapStatus reports paid only for a found transaction; a non-zero result
// (for example an unknown bill number) stays pending.
func mapStatus(res sdk.UpointCheckTransactionResponse) core.Status {
	if res.Result == 0 && res.ReceiptID != "" {
		return core.StatusPaid
	}
	return core.StatusPending
}

// call wraps an SDK call. The SDK dereferences a nil *http.Response when the
// request fails in transport, and returns an empty error for non-200 replies,
// so both are turned into readable provider errors here.
func call[T any](op string, fn func() (T, error)) (res T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = core.Wrap(core.ProviderUPoint, op, fmt.Errorf("request failed: %v", r))
		}
	}()
	res, err = fn()
	if err != nil {
		if err.Error() == "" {
			err = errors.New("non-200 HTTP response")
		}
		return res, core.Wrap(core.ProviderUPoint, op, err)
	}
	return res, nil
}

func resultError(op string, result float64, message string) error {
	return core.Wrap(core.ProviderUPoint, op, fmt.Errorf("result %v: %s", result, message))
}

func extraAmount(extra map[string]string, key string, def float64) (float64, error) {
	s := extra[key]
	if s == "" {
		return def, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%w: Extra[%q] %q is not a non-negative number", core.ErrInvalidInput, key, s)
	}
	return v, nil
}
