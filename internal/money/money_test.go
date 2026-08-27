package money

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCurrencyValid(t *testing.T) {
	assert.True(t, VND.Valid())
	assert.True(t, USD.Valid())
	assert.False(t, Currency("XYZ").Valid())
	assert.False(t, Currency("").Valid())
	assert.Equal(t, "VND", VND.String())
}

func TestAmountHelpers(t *testing.T) {
	assert.True(t, Amount(1).IsPositive())
	assert.False(t, Amount(0).IsPositive())
	assert.False(t, Amount(-1).IsPositive())
	assert.Equal(t, int64(150000), Amount(150000).Int64())
	assert.Equal(t, "150000", Amount(150000).String())
}

func TestFormat(t *testing.T) {
	assert.Equal(t, "150000 VND", Format(150000, VND))
	assert.Equal(t, "19.99 USD", Format(1999, USD))
	assert.Equal(t, "0.05 USD", Format(5, USD))
	assert.Equal(t, "-19.99 USD", Format(-1999, USD))
	assert.Equal(t, "100 XYZ", Format(100, "XYZ"), "an unknown currency falls back to minor units")
}

func TestFee(t *testing.T) {
	tests := []struct {
		name   string
		amount Amount
		bps    int
		want   Amount
	}{
		{name: "2 percent of 100000", amount: 100_000, bps: 200, want: 2_000},
		{name: "2 percent of 150000", amount: 150_000, bps: 200, want: 3_000},
		{name: "zero rate", amount: 100_000, bps: 0, want: 0},
		{name: "negative rate", amount: 100_000, bps: -100, want: 0},
		{name: "zero amount", amount: 0, bps: 200, want: 0},
		{name: "negative amount", amount: -100, bps: 200, want: 0},
		// 1 * 200 / 10000 = 0.02, truncated to 0: rounding never takes from
		// the merchant.
		{name: "rounds down", amount: 1, bps: 200, want: 0},
		{name: "rounds down at 149", amount: 149, bps: 200, want: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Fee(tc.amount, tc.bps))
		})
	}
}

// TestFeeNeverExceedsAmount is the property the ledger depends on: a capture
// entry group credits amount-fee to the merchant, which must not go negative.
func TestFeeNeverExceedsAmount(t *testing.T) {
	for _, amount := range []Amount{1, 99, 100, 12_345, 1_000_000} {
		for _, bps := range []int{0, 1, 200, 5_000, 10_000} {
			fee := Fee(amount, bps)
			assert.LessOrEqual(t, fee, amount, "fee %d exceeds amount %d at %d bps", fee, amount, bps)
			assert.GreaterOrEqual(t, fee, Amount(0))
		}
	}
}
