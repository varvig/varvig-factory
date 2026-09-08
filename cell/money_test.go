package cell

import (
	"errors"
	"math"
	"strconv"
	"testing"
)

// TestParseIsExactWhereFloatIsNot is the reason this type parses digits rather
// than calling ParseFloat: the float path loses a cent on the way in, before any
// arithmetic has happened.
//
// Which written amounts survive that path is a property of binary rounding and
// not of anything a reader could predict — 355.40 happens to come back correct,
// 8.20 does not — which is the argument against relying on it at all rather than
// an argument about these particular numbers.
func TestParseIsExactWhereFloatIsNot(t *testing.T) {
	for _, c := range []struct {
		written  string
		want     Money
		viaFloat int64
	}{
		{"8.20", 820, 819},
		{"1.15", 115, 114},
		{"0.29", 29, 28},
		{"4.35", 435, 434},
		{"16.08", 1608, 1607},
	} {
		got, err := ParseMoney(c.written)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("ParseMoney(%q) = %d, want %d", c.written, got, c.want)
		}
		f, err := strconv.ParseFloat(c.written, 64)
		if err != nil {
			t.Fatal(err)
		}
		if viaFloat := int64(f * 100); viaFloat != c.viaFloat {
			t.Errorf("the float path for %q gives %d; this test is calibrated to %d and needs rechecking",
				c.written, viaFloat, c.viaFloat)
		} else if viaFloat == int64(c.want) {
			t.Errorf("%q was chosen because the float path loses a cent on it, and it no longer does", c.written)
		}
	}
}

func TestMoneyRoundTrips(t *testing.T) {
	for _, s := range []string{
		"0.00", "0.01", "0.10", "1.00", "355.40", "1000.00", "-12.34", "99999999.99",
	} {
		m, err := ParseMoney(s)
		if err != nil {
			t.Fatalf("ParseMoney(%q): %v", s, err)
		}
		if got := m.String(); got != s {
			t.Errorf("ParseMoney(%q).String() = %q", s, got)
		}
	}
}

func TestMoneyAcceptsShortAndSignedForms(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Money
	}{
		{"5", 500}, {"5.", 0}, {"5.5", 550}, {".5", 50}, {"+5.50", 550},
		{"-0.01", -1}, {" 12.34 ", 1234},
	} {
		got, err := ParseMoney(c.in)
		if c.in == "5." {
			if err == nil {
				t.Errorf("ParseMoney(%q) should refuse a decimal point with no digits", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMoney(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMoney(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestAThirdDecimalIsRefused: rounding a written amount is how a disagreement
// about what the currency is turns into a payment.
func TestAThirdDecimalIsRefused(t *testing.T) {
	for _, s := range []string{"1.005", "0.001", "355.401"} {
		if _, err := ParseMoney(s); !errors.Is(err, ErrMoneyFormat) {
			t.Errorf("ParseMoney(%q) should refuse a third decimal place, got %v", s, err)
		}
	}
}

func TestMoneyRefusesNonsense(t *testing.T) {
	for _, s := range []string{"", "  ", "abc", "1.2.3", "1,00", "-", "+", "1e3", "0x10"} {
		if m, err := ParseMoney(s); err == nil {
			t.Errorf("ParseMoney(%q) = %d, want a refusal", s, m)
		}
	}
}

// TestOverflowIsRefusedNotWrapped: a wrapped amount is a negative lease, and a
// negative lease is headroom out of nowhere.
func TestOverflowIsRefusedNotWrapped(t *testing.T) {
	huge := strconv.FormatInt(math.MaxInt64, 10) + ".00"
	if m, err := ParseMoney(huge); err == nil {
		t.Errorf("ParseMoney(%q) = %d, want a refusal rather than a wrap", huge, m)
	}
}

// TestTheArithmeticThatBrokeFloats is the defect, stated as a test: a hold
// released in parts must return to exactly zero, and no part may be refused on
// the way.
func TestTheArithmeticThatBrokeFloats(t *testing.T) {
	held := Money(30) // 0.30
	part := Money(10) // 0.10
	for i := 1; i <= 3; i++ {
		if part > held {
			t.Fatalf("release %d of %s against a hold with %s left was refused", i, part, held)
		}
		held -= part
	}
	if held != 0 {
		t.Errorf("after releasing the whole hold, %s remains; want exactly zero", held)
	}

	// And the accumulation direction, which left a phantom hold in floats.
	var reserved Money
	for i := 0; i < 3; i++ {
		reserved += part
	}
	if reserved != 30 {
		t.Errorf("three holds of %s total %s, want 0.30", part, reserved)
	}
	if reserved-Money(30) != 0 {
		t.Errorf("releasing the total leaves %s, want exactly zero", reserved-Money(30))
	}
}
