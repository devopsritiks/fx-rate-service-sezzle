// Package money provides decimal-safe helpers for currency math.
// We never use float64 for money: binary floating point cannot represent
// most decimal fractions exactly, which leads to rounding drift that is
// unacceptable in a financial system. shopspring/decimal does exact
// base-10 arithmetic instead.
package money

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// DisplayPlaces is the number of decimal places we round converted
// amounts to for display/response purposes. Currency-specific minor
// unit counts (e.g. JPY has 0, not 2) are a known simplification left
// for a later phase.
const DisplayPlaces = 2

// ParseAmount parses a user-supplied amount string into a decimal,
// rejecting negative, zero, non-numeric, or unreasonably large input.
func ParseAmount(s string) (decimal.Decimal, error) {
	if s == "" {
		return decimal.Decimal{}, fmt.Errorf("amount is required")
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("amount must be a valid number")
	}
	if d.Sign() <= 0 {
		return decimal.Decimal{}, fmt.Errorf("amount must be greater than zero")
	}
	// Guard against absurd input (e.g. 1e9999) before it reaches math/formatting.
	if d.Exponent() < -6 || d.NumDigits() > 18 {
		return decimal.Decimal{}, fmt.Errorf("amount is out of allowed range")
	}
	return d, nil
}

// Convert multiplies amount by rate and rounds half-up to DisplayPlaces.
// Half-up (round 0.5 away from zero) is the conventional rounding rule
// for currency display.
func Convert(amount, rate decimal.Decimal) decimal.Decimal {
	return amount.Mul(rate).Round(DisplayPlaces)
}
