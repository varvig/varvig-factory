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
// # The unit is not part of the value
//
// Money is a count of minor units and nothing else: it does not know whether it
// is euros or yen, and it cannot, because the objects that carry an amount carry
// the unit beside it. That is why rendering takes one — In(unit) — and why
// String, which cannot, is documented as the fallback rather than the norm.
// Digits knows how many decimal places each currency is written with, so ¥1000
// prints as 1000 and €10.00 as 10.00.
type Money int64

// ErrMoneyFormat is returned for text that does not name an amount.
var ErrMoneyFormat = errors.New("cell: not an amount")

// String renders an amount at DefaultDigits, for the cases with no unit in hand.
//
// Prefer In: this method cannot know the currency, so it is right for most and
// wrong for yen. It exists because fmt needs something for %s and because a
// message without a unit beside it has nothing better to offer — not as the
// normal way to print money.
func (m Money) String() string { return m.render(DefaultDigits) }

// In renders an amount in its own unit, which is the honest way to print one.
//
// Money is an integer of minor units and carries no currency, so the unit has to
// come from the object holding it — a lease's Unit, a reservation's. Every
// message in this module that prints an amount has one to hand.
func (m Money) In(unit string) string { return m.render(Digits(unit)) }

// render splits the integer rather than dividing in floating point, so what is
// printed is what is stored.
func (m Money) render(digits int) string {
	neg := m < 0
	v := int64(m)
	if neg {
		v = -v
	}
	scale := int64(1)
	for i := 0; i < digits; i++ {
		scale *= 10
	}
	var s string
	if digits == 0 {
		s = strconv.FormatInt(v, 10)
	} else {
		frac := strconv.FormatInt(v%scale, 10)
		for len(frac) < digits {
			frac = "0" + frac
		}
		s = strconv.FormatInt(v/scale, 10) + "." + frac
	}
	if neg {
		return "-" + s
	}
	return s
}

// ParseMoney reads a written amount at DefaultDigits. See ParseIn for the form
// that knows the currency.
func ParseMoney(s string) (Money, error) { return parse(s, DefaultDigits) }

// ParseIn reads a written amount in the minor units of unit.
//
// "1000" in JPY is 1000 minor units, not 100000: a currency with no minor unit
// has its major and minor units the same size, and treating them as different by
// a factor of a hundred is the whole reason this takes a unit.
func ParseIn(unit, s string) (Money, error) { return parse(s, Digits(unit)) }

// parse reads the digits directly rather than going through float64, because
// that path loses money on the way in, before any arithmetic has happened.
// strconv.ParseFloat("8.20") yields 8.1999999999999992895, and multiplying by
// 100 and truncating gives 819 — an everyday price, a cent short. The same
// holds for 1.15, 0.29, 4.35 and 16.08 among many others; which values survive
// is a property of binary rounding and not of anything a reader could predict,
// which is the argument against relying on it at all.
//
// More decimal places than the currency has is refused rather than rounded. A
// written amount with an extra digit means the writer and this code disagree
// about what the currency is, and silently dropping it is how that disagreement
// becomes a payment.
func parse(s string, digits int) (Money, error) {
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
	var minor int64
	if hasFrac {
		switch {
		case len(frac) == 0:
			return 0, fmt.Errorf("%w: %q has a decimal point and no digits after it", ErrMoneyFormat, s)
		case digits == 0:
			return 0, fmt.Errorf("%w: %q has decimal places but this unit has none", ErrMoneyFormat, s)
		case len(frac) > digits:
			return 0, fmt.Errorf("%w: %q has more than %d decimal place(s); this unit is counted in %d and a written amount will not be rounded", ErrMoneyFormat, s, digits, digits)
		}
		for len(frac) < digits {
			frac += "0"
		}
		minor, err = strconv.ParseInt(frac, 10, 64)
		if err != nil || minor < 0 {
			return 0, fmt.Errorf("%w: %q", ErrMoneyFormat, s)
		}
	}
	scale := int64(1)
	for i := 0; i < digits; i++ {
		scale *= 10
	}
	// Overflow is refused rather than wrapped: a wrapped amount is a negative
	// lease, and a negative lease is headroom out of nowhere.
	if units > (1<<63-1-minor)/scale || units < (-(1<<63)+minor)/scale {
		return 0, fmt.Errorf("%w: %q does not fit in an int64 of minor units", ErrMoneyFormat, s)
	}
	total := units*scale + minor
	if neg {
		total = -total
	}
	return Money(total), nil
}
