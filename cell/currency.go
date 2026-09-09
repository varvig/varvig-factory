package cell

import "strings"

// How many minor units make one major unit of a currency.
//
// Money is an integer of minor units and says nothing about how to write one
// down. Two decimal places is right for most currencies and wrong for several
// that are in daily use: yen and won have no minor unit at all, and a handful of
// dinars have three. Rendering ¥1000 as "10.00" is not a rounding error, it is a
// different number by a factor of a hundred.
//
// # Where the data comes from, and why so little of it
//
// This is ISO 4217's minor-unit column for the currencies that are *not* two —
// the exceptions, not the world. A full currency table would be data this module
// has no way to keep current, and staleness in a table like that is worse than
// its absence: a currency that redenominates leaves a wrong answer that looks
// authoritative. The exceptions listed here have been stable for decades, and
// anything absent is assumed to have two, which is both the common case and the
// safe default for a unit this module has never heard of — a made-up unit like
// "credits" behaves like a currency with cents.
//
// An envelope or lease that needs something else says so: Digits is the default,
// not the authority. Where a caller knows better it passes its own.
var exponents = map[string]int{
	// No minor unit.
	"BIF": 0, "CLP": 0, "DJF": 0, "GNF": 0, "ISK": 0, "JPY": 0, "KMF": 0,
	"KRW": 0, "PYG": 0, "RWF": 0, "UGX": 0, "UYI": 0, "VND": 0, "VUV": 0,
	"XAF": 0, "XOF": 0, "XPF": 0,
	// Three.
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
	// Four.
	"CLF": 4, "UYW": 4,
}

// DefaultDigits is what an unlisted unit is assumed to have.
const DefaultDigits = 2

// Digits reports how many decimal places a unit is written with.
//
// The lookup is case-insensitive on the ISO code so a config that says "eur"
// behaves like one that says "EUR"; a unit that is not a currency code at all
// falls through to the default, which is the intended behaviour rather than a
// failure to recognise it.
func Digits(unit string) int {
	if n, ok := exponents[strings.ToUpper(strings.TrimSpace(unit))]; ok {
		return n
	}
	return DefaultDigits
}
