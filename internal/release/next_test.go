package release

import "testing"

// versions is a test helper: tags in, parsed versions out.
func versions(t *testing.T, tags ...string) []Version {
	t.Helper()
	var out []Version
	for _, tag := range tags {
		v, ok := ParseVersion(tag)
		if !ok {
			t.Fatalf("test setup: %q is not a version", tag)
		}
		out = append(out, v)
	}
	return out
}

func TestLatestStable(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want string // "" means the zero Version
	}{
		{"none", nil, ""},
		{"single", []string{"v1.2.3"}, "v1.2.3"},
		{"highest of several, order independent", []string{"v1.2.3", "v2.0.0", "v1.9.9"}, "v2.0.0"},
		// Prereleases must not move the base, or publishing an alpha would
		// change what the next alpha counts from.
		{"prereleases ignored", []string{"v1.2.3", "v1.3.0-alpha.1", "v1.3.0-beta.2"}, "v1.2.3"},
		{"only prereleases means none", []string{"v1.3.0-alpha.1"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LatestStable(versions(t, tt.tags...))
			if tt.want == "" {
				if !got.IsZero() {
					t.Errorf("LatestStable() = %s, want the zero Version", got)
				}
				return
			}
			if got.String() != tt.want {
				t.Errorf("LatestStable() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNextStable(t *testing.T) {
	tests := []struct {
		name    string
		latest  string // "" means no tags yet
		commits []Commit
		want    string // "" means no release
	}{
		{
			"feat bumps minor",
			"v1.2.3", []Commit{{Subject: "feat: add Glob"}}, "v1.3.0",
		},
		{
			"fix bumps patch",
			"v1.2.3", []Commit{{Subject: "fix: roll back"}}, "v1.2.4",
		},
		{
			"breaking bumps major and zeroes the rest",
			"v1.2.3", []Commit{{Subject: "feat!: drop a flag"}}, "v2.0.0",
		},
		{
			"minor zeroes patch",
			"v1.2.9", []Commit{{Subject: "feat: add Glob"}}, "v1.3.0",
		},
		{
			"nothing releasable means no release",
			"v1.2.3", []Commit{{Subject: "docs: typo"}, {Subject: "chore: deps"}}, "",
		},
		{
			"no commits means no release, which makes a re-run safe",
			"v1.2.3", nil, "",
		},
		{
			"first release seeds at v0.1.0 regardless of bump size",
			"", []Commit{{Subject: "feat!: everything"}}, "v0.1.0",
		},
		{
			"first release still needs something releasable",
			"", []Commit{{Subject: "docs: readme"}}, "",
		},
		{
			"breaking from v0.x goes to v1.0.0",
			"v0.1.0", []Commit{{Subject: "feat!: drop a flag"}}, "v1.0.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var latest Version
			if tt.latest != "" {
				latest = versions(t, tt.latest)[0]
			}
			got, ok := Next(latest, tt.commits, Stable, nil)
			if tt.want == "" {
				if ok {
					t.Errorf("Next() = %s, true; want no release", got)
				}
				return
			}
			if !ok {
				t.Fatalf("Next() returned no release, want %s", tt.want)
			}
			if got.String() != tt.want {
				t.Errorf("Next() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNextPrerelease(t *testing.T) {
	tests := []struct {
		name     string
		latest   string
		commits  []Commit
		channel  Channel
		existing []string
		want     string
	}{
		{
			"first alpha of a minor bump",
			"v1.2.3", []Commit{{Subject: "feat: add Glob"}}, Alpha,
			[]string{"v1.2.3"}, "v1.3.0-alpha.1",
		},
		{
			"counter continues past existing alphas",
			"v1.2.3", []Commit{{Subject: "feat: add Glob"}}, Alpha,
			[]string{"v1.2.3", "v1.3.0-alpha.1", "v1.3.0-alpha.2"}, "v1.3.0-alpha.3",
		},
		{
			"beta counts separately from alpha",
			"v1.2.3", []Commit{{Subject: "feat: add Glob"}}, Beta,
			[]string{"v1.2.3", "v1.3.0-alpha.1", "v1.3.0-alpha.2"}, "v1.3.0-beta.1",
		},
		{
			"counter restarts for a new base once the old one shipped",
			"v1.3.0", []Commit{{Subject: "feat: another"}}, Alpha,
			[]string{"v1.2.3", "v1.3.0-alpha.1", "v1.3.0-alpha.2", "v1.3.0"}, "v1.4.0-alpha.1",
		},
		{
			"a prerelease of a different base does not advance this counter",
			"v1.2.3", []Commit{{Subject: "feat: add Glob"}}, Alpha,
			[]string{"v1.2.3", "v9.0.0-alpha.7"}, "v1.3.0-alpha.1",
		},
		{
			"breaking commits give a major base",
			"v1.2.3", []Commit{{Subject: "feat!: drop a flag"}}, Beta,
			[]string{"v1.2.3"}, "v2.0.0-beta.1",
		},
		{
			// A manual dispatch is an explicit intent to ship something for
			// testing, so it falls back to a patch rather than refusing.
			"nothing releasable falls back to a patch base",
			"v1.2.3", []Commit{{Subject: "refactor: tidy"}}, Alpha,
			[]string{"v1.2.3"}, "v1.2.4-alpha.1",
		},
		{
			"no commits at all also falls back to a patch base",
			"v1.2.3", nil, Alpha,
			[]string{"v1.2.3"}, "v1.2.4-alpha.1",
		},
		{
			"first prerelease of an untagged repo seeds at the seed version",
			"", []Commit{{Subject: "feat: first"}}, Alpha,
			nil, "v0.1.0-alpha.1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var latest Version
			if tt.latest != "" {
				latest = versions(t, tt.latest)[0]
			}
			got, ok := Next(latest, tt.commits, tt.channel, versions(t, tt.existing...))
			if !ok {
				t.Fatalf("Next() returned no release, want %s", tt.want)
			}
			if got.String() != tt.want {
				t.Errorf("Next() = %s, want %s", got, tt.want)
			}
		})
	}
}
