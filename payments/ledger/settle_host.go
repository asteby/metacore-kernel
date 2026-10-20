package ledger

import (
	"errors"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
)

// SettleKey is the host-implemented payments.Settle: the mixed-collection
// rule (only cash gives change; a cash shortfall is absorbed up to the org
// tolerance, MaxCashTolerance and MaxRoundingPercent) as ONE implementation
// for every channel (POS, cash desk, kiosk). Pure: no organization, no
// ledger, no writes; a guest asks it before writing its tenders.
const SettleKey = "payments.settle"

// Settle error codes (capability.CodedError.Code of the result).
const (
	CodeSettleExceedsTotal     = "exceeds_total"
	CodeSettleNothingDue       = "nothing_due"
	CodeSettleTenderMismatch   = "tender_mismatch"
	CodeSettleInvalidTolerance = "invalid_tolerance"
	CodeSettleNegativeAmount   = "negative_amount"
	CodeSettleNotQuantized     = "not_quantized"
)

func settleContract() capability.Contract {
	dec := func(name, doc string) capability.Field { return o(name, capability.TypeDecimal, doc) }
	return capability.Contract{
		Key: SettleKey, Kind: capability.KindQuery, Cardinality: capability.One,
		Doc: "apply a collection to a document: what it pays, the cash change, the cash rounding absorbed and what is left (payments.Settle; pure, writes nothing)",
		Input: in(
			r("currency", capability.TypeCurrency, ""),
			capability.Field{Name: "total", Type: capability.TypeDecimal, Required: true, Doc: "document total from its STORED lines"},
			dec("already_paid", "what counts as paid before this collection (server side)"),
			dec("non_cash", "card / transfer / wallet part; never above what is due"),
			dec("cash", "cash tendered; the only part that gives change"),
			dec("cash_tolerance", "org cash rounding tolerance, within MaxCashTolerance of the currency"),
		),
		Output: out(
			r("due", capability.TypeDecimal, "owed before this collection"),
			r("applied", capability.TypeDecimal, "what this collection contributes"),
			r("change", capability.TypeDecimal, "cash handed back (never above the cash tendered)"),
			r("rounding", capability.TypeDecimal, "cash shortfall absorbed under the tolerance"),
			r("remaining", capability.TypeDecimal, "still owed (partial payment)"),
			r("settled", capability.TypeBoolean, "nothing is owed any more"),
		),
	}
}

// settle answers SettleKey.
func settle(in map[string]any) (any, error) {
	s, err := payments.Settle(payments.Collection{
		Currency: str(in, "currency"), Total: str(in, "total"), AlreadyPaid: str(in, "already_paid"),
		NonCash: str(in, "non_cash"), Cash: str(in, "cash"), CashTolerance: str(in, "cash_tolerance"),
	})
	if err != nil {
		return nil, settleError(err)
	}
	return map[string]any{
		"due": s.Due.String(), "applied": s.Applied.String(), "change": s.Change.String(),
		"rounding": s.Rounding.String(), "remaining": s.Remaining.String(), "settled": s.Settled,
	}, nil
}

func settleError(err error) error {
	code := CodeInvalidInput
	switch {
	case errors.Is(err, payments.ErrExceedsTotal):
		code = CodeSettleExceedsTotal
	case errors.Is(err, payments.ErrNothingDue):
		code = CodeSettleNothingDue
	case errors.Is(err, payments.ErrTenderMismatch):
		code = CodeSettleTenderMismatch
	case errors.Is(err, payments.ErrInvalidTolerance):
		code = CodeSettleInvalidTolerance
	case errors.Is(err, payments.ErrNegativeAmount):
		code = CodeSettleNegativeAmount
	case errors.Is(err, payments.ErrNotQuantized):
		code = CodeSettleNotQuantized
	}
	return fail(code, err)
}
