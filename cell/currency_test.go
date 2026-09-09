package cell

import "testing"

// TestYenIsNotRenderedAsCents is the bug this table exists for. ¥1000 written
// as "10.00" is not a rounding error, it is a different number by a factor of a
// hundred.
func TestYenIsNotRenderedAsCents(t *testing.T) {
	m := Money(1000)
	if got := m.In("JPY"); got != "1000" {
		t.Errorf("1000 minor units in JPY = %q, want \"1000\"", got)
	}
	if got := m.In("EUR"); got != "10.00" {
		t.Errorf("1000 minor units in EUR = %q, want \"10.00\"", got)
	}
	if got := m.In("KWD"); got != "1.000" {
		t.Errorf("1000 minor units in KWD = %q, want \"1.000\"", got)
	}
}

// TestParsingRespectsTheUnit: "1000" in yen is a thousand yen, not ten.
func TestParsingRespectsTheUnit(t *testing.T) {
	for _, c := range []struct {
		unit, in string
		want     Money
	}{
		{"JPY", "1000", 1000},
		{"EUR", "1000", 100000},
		{"KWD", "1.500", 1500},
		{"eur", "10.00", 1000}, // case-insensitive
		{"credits", "10.00", 1000},
	} {
		got, err := ParseIn(c.unit, c.in)
		if err != nil {
			t.Errorf("ParseIn(%q, %q): %v", c.unit, c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseIn(%q, %q) = %d, want %d", c.unit, c.in, got, c.want)
		}
	}
}

// TestDecimalsAreRefusedForAUnitThatHasNone: "10.50" in yen means the writer and
// this code disagree about the currency, and rounding it away is how that
// becomes a payment.
func TestDecimalsAreRefusedForAUnitThatHasNone(t *testing.T) {
	if m, err := ParseIn("JPY", "10.50"); err == nil {
		t.Errorf("ParseIn(JPY, \"10.50\") = %d, want a refusal", m)
	}
	if _, err := ParseIn("KWD", "1.5000"); err == nil {
		t.Error("a fourth decimal place in a three-decimal currency should be refused")
	}
}

// TestRoundTripInEveryShape.
func TestRoundTripInEveryShape(t *testing.T) {
	for _, unit := range []string{"EUR", "JPY", "KWD", "unknown-unit"} {
		for _, m := range []Money{0, 1, 999, 100000, -4207} {
			s := m.In(unit)
			back, err := ParseIn(unit, s)
			if err != nil {
				t.Errorf("ParseIn(%q, %q): %v", unit, s, err)
				continue
			}
			if back != m {
				t.Errorf("%d in %s rendered %q and parsed back as %d", m, unit, s, back)
			}
		}
	}
}

// TestAnUnknownUnitGetsTheCommonCase: a made-up unit behaves like a currency
// with cents, which is both the common shape and the safe assumption for
// something this module has never heard of.
func TestAnUnknownUnitGetsTheCommonCase(t *testing.T) {
	if got := Digits("credits"); got != DefaultDigits {
		t.Errorf("Digits(credits) = %d, want %d", got, DefaultDigits)
	}
	if got := Digits(""); got != DefaultDigits {
		t.Errorf("Digits(\"\") = %d, want %d", got, DefaultDigits)
	}
}
