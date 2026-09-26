package safety

import "testing"

func TestRiskTierString(t *testing.T) {
	cases := map[RiskTier]string{
		Safe:      "safe",
		Moderate:  "moderate",
		Dangerous: "dangerous",
	}
	for tier, want := range cases {
		if got := tier.String(); got != want {
			t.Errorf("RiskTier(%d).String() = %q, want %q", int(tier), got, want)
		}
	}
}

func TestParseRiskTier(t *testing.T) {
	tests := []struct {
		in      string
		want    RiskTier
		wantErr bool
	}{
		{"safe", Safe, false},
		{"Moderate", Moderate, false},
		{"  dangerous  ", Dangerous, false},
		{"catastrophic", Dangerous, true},
		{"", Dangerous, true},
	}
	for _, tt := range tests {
		got, err := ParseRiskTier(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseRiskTier(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("ParseRiskTier(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
