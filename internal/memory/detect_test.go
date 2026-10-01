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
		got := DetectContext(dir)
		if !contains(got.Tags, c.want) {
			t.Errorf("%s -> Tags %v, want it to contain %q", c.marker, got.Tags, c.want)
		}
	}
}

func TestDetectContext_NoMarkersNoRemoteIsEmpty(t *testing.T) {
	got := DetectContext(t.TempDir())
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

// Org detection walks up (git does it) and so does project config, so marker
// detection has to as well — otherwise running from a subdirectory keeps the
// org but silently drops the stack tag, and every tags-filtered memory stops
// applying one level down from the repo root.
func TestDetectContext_FindsStackMarkersFromASubdirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "services", "api", "internal")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	atRoot := DetectContext(root)
	atDeep := DetectContext(deep)

	if !contains(atRoot.Tags, "go") {
		t.Fatalf("root tags = %v, want go", atRoot.Tags)
	}
	if !contains(atDeep.Tags, "go") {
		t.Errorf("subdirectory tags = %v, want go — tag-filtered memories would stop applying", atDeep.Tags)
	}
}

// The search must not escape the repository and pick up a parent project's stack.
func TestDetectContext_MarkerSearchStopsAtTheRepositoryRoot(t *testing.T) {
	outer := t.TempDir()
	if err := os.WriteFile(filepath.Join(outer, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(outer, "vendored")
	if err := os.MkdirAll(filepath.Join(inner, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "go.mod"), []byte("module y\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := DetectContext(inner)
	if !contains(got.Tags, "go") {
		t.Errorf("tags = %v, want go from the inner repo", got.Tags)
	}
	if contains(got.Tags, "node") {
		t.Errorf("tags = %v, leaked node from above the repository boundary", got.Tags)
	}
}
