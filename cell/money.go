package cell

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Money is an amount in the minor units of whatever currency names it — cents
// for EUR, and so on. It is an integer, and that is the whole point.
//
// # Why not float64
//
// Binary floating point cannot represent decimal fractions like 0.10 or 355.40
// exactly. That is usually harmless, because most code only displays the result.
// This code does three things that are not harmless:
//
//   - It **accumulates**. A lease's spend grows by one settlement at a time, and
//     each addition carries its own error.
//   - It relies on **exact round-trips**. A hold of X released again must return
//     the lease to precisely where it started, or the residue reads as an order
//     still outstanding.
//   - It **decides refusals on comparisons**. Lease.Release refuses when the
//     amount exceeds what is held, and that is a spend guard, not a display.
//
// Those three together are the worst case for floats, and it was not theoretical.
// Hold 0.30 and release 0.10 three times, and the third release was refused as a
// double release — with a message reading "releasing 0.1 EUR ... which holds
// only 0.1", because the formatter rounded both sides to the same string. A
// legitimate release refused as a double-spend attempt, explained by a sentence
// that reads as a contradiction.
//
// The mirror case leaves residue instead: three holds of 0.10 against one
// release of 0.30 leaves 5.5e-17 reserved — a phantom hold that any "is anything
// outstanding" check reads as yes, permanently.
//
// In minor units all of that is exact: addition, subtraction, round-trips and
// comparisons. The care moves to the edges, which is where it belongs — parsing
// a written amount, and rendering one.
//
// # Two decimal places
//
// String and ParseMoney assume the currency has two decimal places. That is
// right for EUR, USD and most others, and wrong for JPY (zero) and a handful
// with three. Modelling the exponent per currency is a real thing to do and is
// deliberately not done here: it needs currency data this module does not have,
// and getting the arithmetic exact is worth having without it. What it costs
// today is a display bug for such a currency, never a spend error — the stored
// integer is still whatever was put in it.
type Money int64

// ErrMoneyFormat is returned for text that does not name an amount.
var ErrMoneyFormat = errors.New("cell: not an amount")

// String renders an amount the way a person expects to read it.
//
// It is exact: the integer is split, never divided in floating point, so what is
// printed is what is stored.
func (m Money) String() string {
	neg := m < 0
	v := int64(m)
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%d.%02d", v/100, v%100)
	if neg {
		return "-" + s
	}
	return s
}

// ParseMoney reads a written amount into minor units.
//
// It parses the digits directly rather than going through float64, because that
// path loses money on the way in, before any arithmetic has happened.
// strconv.ParseFloat("8.20") yields 8.1999999999999992895, and multiplying by
// 100 and truncating gives 819 — an everyday price, a cent short. The same
// holds for 1.15, 0.29, 4.35 and 16.08 among many others; which values survive
// is a property of binary rounding and not of anything a reader could predict,
// which is the argument against relying on it at all.
//
// More than two decimal places is refused rather than rounded. A written amount
// with a third digit means the writer and this code disagree about what the
// currency is, and silently dropping it is how that disagreement becomes a
// payment.
func ParseMoney(s string) (Money, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("%w: %q", ErrMoneyFormat, s)
	}
	neg := false
	switch t[0] {
	case '-':
		neg, t = true, t[1:]
	case '+':
		t = t[1:]
	}
	whole, frac, hasFrac := strings.Cut(t, ".")
	if whole == "" && !hasFrac {
		return 0, fmt.Errorf("%w: %q", ErrMoneyFormat, s)
	}
	if whole == "" {
		whole = "0"
	}
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrMoneyFormat, s)
	}
	var cents int64
	if hasFrac {
		switch len(frac) {
		case 0:
			return 0, fmt.Errorf("%w: %q has a decimal point and no digits after it", ErrMoneyFormat, s)
		case 1:
			frac += "0"
		case 2:
		default:
			return 0, fmt.Errorf("%w: %q has more than two decimal places; this module counts in hundredths and will not round a written amount", ErrMoneyFormat, s)
		}
		cents, err = strconv.ParseInt(frac, 10, 64)
		if err != nil || cents < 0 {
			return 0, fmt.Errorf("%w: %q", ErrMoneyFormat, s)
		}
	}
	// Overflow is refused rather than wrapped: a wrapped amount is a negative
	// lease, and a negative lease is headroom out of nowhere.
	if units > (1<<63-1-cents)/100 || units < (-(1<<63)+cents)/100 {
		return 0, fmt.Errorf("%w: %q does not fit in an int64 of minor units", ErrMoneyFormat, s)
	}
	total := units*100 + cents
	if neg {
		total = -total
	}
	return Money(total), nil
}
