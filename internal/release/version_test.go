package release

import "testing"

func TestParseVersion(t *testing.T) {
	tests := []struct {
		tag  string
		want Version
		ok   bool
	}{
		{"v1.2.3", Version{Major: 1, Minor: 2, Patch: 3}, true},
		{"v0.1.0", Version{Minor: 1}, true},
		{"v10.20.30", Version{Major: 10, Minor: 20, Patch: 30}, true},
		{"v1.3.0-alpha.2", Version{Major: 1, Minor: 3, Channel: Alpha, Counter: 2}, true},
		{"v1.3.0-beta.11", Version{Major: 1, Minor: 3, Channel: Beta, Counter: 11}, true},
		// A repository may carry tags that are not versions at all.
		{"nightly", Version{}, false},
		{"1.2.3", Version{}, false},
		{"v1.2", Version{}, false},
		{"v1.2.3-rc.1", Version{}, false},
		{"v1.2.3-alpha", Version{}, false},
		{"", Version{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			got, ok := ParseVersion(tt.tag)
			if ok != tt.ok {
				t.Fatalf("ParseVersion(%q) ok = %v, want %v", tt.tag, ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("ParseVersion(%q) = %+v, want %+v", tt.tag, got, tt.want)
			}
		})
	}
}

func TestVersionString(t *testing.T) {
	tests := []struct {
		v    Version
		want string
	}{
		{Version{Major: 1, Minor: 2, Patch: 3}, "v1.2.3"},
		{Version{Minor: 1}, "v0.1.0"},
		{Version{Major: 1, Minor: 3, Channel: Alpha, Counter: 1}, "v1.3.0-alpha.1"},
		{Version{Major: 2, Channel: Beta, Counter: 7}, "v2.0.0-beta.7"},
	}
	for _, tt := range tests {
		if got := tt.v.String(); got != tt.want {
			t.Errorf("%+v String() = %q, want %q", tt.v, got, tt.want)
		}
	}
}

func TestVersionStringRoundTripsThroughParse(t *testing.T) {
	for _, tag := range []string{"v0.1.0", "v1.2.3", "v1.3.0-alpha.2", "v2.0.0-beta.11"} {
		v, ok := ParseVersion(tag)
		if !ok {
			t.Fatalf("ParseVersion(%q) failed", tag)
		}
		if got := v.String(); got != tag {
			t.Errorf("round trip of %q gave %q", tag, got)
		}
	}
}

func TestVersionLess(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"patch", "v1.2.3", "v1.2.4", true},
		{"minor outranks patch", "v1.2.9", "v1.3.0", true},
		{"major outranks minor", "v1.9.0", "v2.0.0", true},
		{"equal is not less", "v1.2.3", "v1.2.3", false},
		{"descending is not less", "v1.2.4", "v1.2.3", false},
		// A prerelease precedes the release it leads to.
		{"prerelease before its base", "v1.3.0-alpha.1", "v1.3.0", true},
		{"base not before its prerelease", "v1.3.0", "v1.3.0-alpha.1", false},
		{"alpha before beta", "v1.3.0-alpha.9", "v1.3.0-beta.1", true},
		{"counter orders within a channel", "v1.3.0-alpha.2", "v1.3.0-alpha.10", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := ParseVersion(tt.a)
			b, _ := ParseVersion(tt.b)
			if got := a.Less(b); got != tt.want {
				t.Errorf("%s.Less(%s) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestVersionBaseAndIsZero(t *testing.T) {
	v, _ := ParseVersion("v1.3.0-alpha.2")
	if base := v.Base(); base != (Version{Major: 1, Minor: 3}) {
		t.Errorf("Base() = %+v, want v1.3.0", base)
	}
	if !(Version{}).IsZero() {
		t.Error("zero Version should report IsZero")
	}
	if v.IsZero() {
		t.Error("v1.3.0-alpha.2 should not report IsZero")
	}
}
