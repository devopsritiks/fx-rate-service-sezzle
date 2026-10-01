package money

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestParseAmount(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid", "100.00", false},
		{"valid integer", "100", false},
		{"empty", "", true},
		{"zero", "0", true},
		{"negative", "-5.00", true},
		{"not a number", "abc", true},
		{"too many digits", "123456789012345678901", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAmount(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ParseAmount(%q) error = %v, wantErr %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestConvert(t *testing.T) {
	amount := decimal.RequireFromString("100.00")
	rate := decimal.RequireFromString("1.4246")

	got := Convert(amount, rate)
	want := decimal.RequireFromString("142.46")

	if !got.Equal(want) {
		t.Errorf("Convert(100.00, 1.4246) = %s, want %s", got, want)
	}
}

func TestConvertRoundsHalfUp(t *testing.T) {
	amount := decimal.RequireFromString("1")
	rate := decimal.RequireFromString("0.005")

	got := Convert(amount, rate)
	want := decimal.RequireFromString("0.01")

	if !got.Equal(want) {
		t.Errorf("Convert(1, 0.005) = %s, want %s", got, want)
	}
}
