package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectContext_StackMarkers(t *testing.T) {
	cases := []struct {
		marker string
		want   string
	}{
		{"go.mod", "go"},
		{"package.json", "node"},
		{"Cargo.toml", "rust"},
		{"pyproject.toml", "python"},
		{"requirements.txt", "python"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, c.marker), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := DetectContext(dir, "")
		if !contains(got.Tags, c.want) {
			t.Errorf("%s -> Tags %v, want it to contain %q", c.marker, got.Tags, c.want)
		}
	}
}

func TestDetectContext_ProjectOrgWinsOverGitRemote(t *testing.T) {
	dir := t.TempDir()
	if got := DetectContext(dir, "explicit-org"); got.Org != "explicit-org" {
		t.Errorf("Org = %q, want the configured value to take precedence", got.Org)
	}
}

func TestDetectContext_NoMarkersNoRemoteIsEmpty(t *testing.T) {
	got := DetectContext(t.TempDir(), "")
	if got.Org != "" {
		t.Errorf("Org = %q, want empty when nothing identifies the org", got.Org)
	}
	if len(got.Tags) != 0 {
		t.Errorf("Tags = %v, want empty", got.Tags)
	}
}

func TestParseOrgFromRemote(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/repo.git":   "acme",
		"https://github.com/acme/repo":       "acme",
		"git@github.com:acme/repo.git":       "acme",
		"ssh://git@gitlab.com/acme/repo.git": "acme",
		"git@bitbucket.org:acme/repo.git":    "acme",
		"":                                   "",
		"not a url":                          "",
	}
	for in, want := range cases {
		if got := parseOrgFromRemote(in); got != want {
			t.Errorf("parseOrgFromRemote(%q) = %q, want %q", in, got, want)
		}
	}
}
