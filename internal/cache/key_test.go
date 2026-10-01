package cache

import "testing"

func TestKey_NormalizesOrderAndCase(t *testing.T) {
	a := Key("usd", []string{"CAD", "eur"})
	b := Key("USD", []string{"eur", "cad"})

	if a != b {
		t.Errorf("expected normalized keys to match: %q != %q", a, b)
	}
}

func TestKey_DedupesSymbols(t *testing.T) {
	a := Key("USD", []string{"CAD", "CAD", "EUR"})
	b := Key("USD", []string{"CAD", "EUR"})

	if a != b {
		t.Errorf("expected duplicate symbols to collapse: %q != %q", a, b)
	}
}

func TestKey_NoSymbols(t *testing.T) {
	if got, want := Key("USD", nil), "USD"; got != want {
		t.Errorf("Key(USD, nil) = %q, want %q", got, want)
	}
}

func TestKey_DifferentBaseDiffers(t *testing.T) {
	a := Key("USD", []string{"CAD"})
	b := Key("EUR", []string{"CAD"})

	if a == b {
		t.Errorf("expected different base currencies to produce different keys, both were %q", a)
	}
}
