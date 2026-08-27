// Package money models monetary values as integer minor units.
//
// Floating point is never used: 0.1 + 0.2 != 0.3 in binary floating point, and
// a payment system that loses a cent per rounding loses trust. An int64 of
// minor units (VND: dong, USD: cent) is exact and holds ~9.2e18 units.
package money

import (
	"fmt"
	"strconv"
)

// Amount is a monetary value in minor units. Always paired with a Currency.
type Amount int64

// Currency is an ISO-4217 alphabetic code.
type Currency string

const (
	VND Currency = "VND"
	USD Currency = "USD"
)

// minorUnits maps a currency to the number of decimal places of its minor unit.
var minorUnits = map[Currency]int{
	VND: 0,
	USD: 2,
}

// Valid reports whether the currency is one this gateway accepts.
func (c Currency) Valid() bool {
	_, ok := minorUnits[c]
	return ok
}

func (c Currency) String() string { return string(c) }

// IsPositive reports whether the amount is strictly greater than zero.
// Every money-moving request must carry a positive amount.
func (a Amount) IsPositive() bool { return a > 0 }

func (a Amount) Int64() int64 { return int64(a) }

func (a Amount) String() string { return strconv.FormatInt(int64(a), 10) }

// Format renders the amount in major units for humans and reports, e.g.
// Format(150000, "VND") == "150000 VND" and Format(1999, "USD") == "19.99 USD".
func Format(a Amount, c Currency) string {
	exp, ok := minorUnits[c]
	if !ok || exp == 0 {
		return fmt.Sprintf("%d %s", int64(a), c)
	}
	div := int64(1)
	for i := 0; i < exp; i++ {
		div *= 10
	}
	neg := ""
	v := int64(a)
	if v < 0 {
		neg, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%0*d %s", neg, v/div, exp, v%div, c)
}

// Fee returns the platform fee for an amount, expressed in basis points
// (1 bps = 0.01%). Integer division truncates, so the fee is always rounded
// down and the merchant is never short-changed by rounding.
func Fee(a Amount, bps int) Amount {
	if a <= 0 || bps <= 0 {
		return 0
	}
	return Amount(int64(a) * int64(bps) / 10_000)
}
