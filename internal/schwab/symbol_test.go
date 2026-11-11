package schwab

import "testing"

// ParseOCC reads the root, the expiration, put or call and the strike of an
// OCC option symbol, whose root is padded to six characters, and refuses
// any other padding, an expiration that is not a date, another letter than
// C or P, and a strike that is not eight digits.
func TestParseOCC(t *testing.T) {
	for _, tt := range []struct {
		symbol string
		want   OCC
	}{
		{"XYZ   260619P00045000", OCC{"XYZ", "2026-06-19", "PUT", 45000}},
		{"XYZ   260918C00050000", OCC{"XYZ", "2026-09-18", "CALL", 50000}},
		{"SPXW  260619C05000000", OCC{"SPXW", "2026-06-19", "CALL", 5000000}},
		{"AAPL1 260619C00180000", OCC{"AAPL1", "2026-06-19", "CALL", 180000}},
		{"ABCDEF270115P00017500", OCC{"ABCDEF", "2027-01-15", "PUT", 17500}},
		{"F     280121C00012345", OCC{"F", "2028-01-21", "CALL", 12345}},
	} {
		if got, ok := ParseOCC(tt.symbol); !ok || got != tt.want {
			t.Errorf("ParseOCC(%q) = %+v, %v; want %+v", tt.symbol, got, ok, tt.want)
		}
	}
	for _, s := range []string{
		"XYZ  260619P00045000", "XYZ    260619P00045000", "XYZ260619P00045000", "xyz   260619P00045000",
		"XYZ   261319P00045000", "XYZ   260231P00045000", "XYZ   260619Q00045000", "XYZ   260619P0004500",
		"XYZ   260619P000450000", "XYZ 06/19/2026 45.00 P", "XYZ", "/ESH25_P5050:XCME", "",
	} {
		if got, ok := ParseOCC(s); ok {
			t.Errorf("ParseOCC(%q) = %+v", s, got)
		}
	}
}

// IsCUSIP takes nine letters and digits whose last is the check digit of
// the others, and nothing else.
func TestIsCUSIP(t *testing.T) {
	for _, s := range []string{"922908769", "30303M102", "00507V109", "037833100", "38259P508"} {
		if !IsCUSIP(s) {
			t.Errorf("IsCUSIP(%q) is false", s)
		}
	}
	for _, s := range []string{
		"922908768", "00507V108", "30303m102", "92290876", "9229087690", "9229 0876", "0XYZ..RI60045000", "AAPL", "",
	} {
		if IsCUSIP(s) {
			t.Errorf("IsCUSIP(%q) is true", s)
		}
	}
}

// SymbolAfterTilde returns what follows a description's last "~" when it
// is a symbol, of any kind, and nothing when there is no "~", nothing after
// it, or text that is no symbol.
func TestSymbolAfterTilde(t *testing.T) {
	for description, want := range map[string]string{
		"W-8 Withholding~BND":               "BND",
		"Qualified Dividend~VTI":            "VTI",
		"Cash In Lieu~BRK.B":                "BRK.B",
		"Dividend~00507V109":                "00507V109",
		"A~B~SWVXX":                         "SWVXX",
		"Expiration~XYZ   260619P00045000":  "XYZ   260619P00045000",
		"Dividend Reinvest":                 "",
		"W-8 Withholding~ACME BANK":         "",
		"W-8 Withholding~":                  "",
		"W-8 Withholding~bnd":               "",
		"W-8 Withholding~BND ":              "",
		"Dividend~00507V108":                "",
		"Expiration~XYZ   261319P00045000":  "",
		"Wire to account 12345678~29470135": "",
		"":                                  "",
	} {
		got, ok := SymbolAfterTilde(description)
		if got != want || ok != (want != "") {
			t.Errorf("SymbolAfterTilde(%q) = %q, %v; want %q", description, got, ok, want)
		}
	}
}
