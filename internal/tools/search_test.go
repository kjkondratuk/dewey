package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
)

// seed creates files under dir, making parent directories as needed.
func seed(t *testing.T, dir string, rel ...string) {
	t.Helper()
	for _, r := range rel {
		full := filepath.Join(dir, r)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGlob_MatchesRecursivelyWithDoubleStar(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "a.go", "sub/b.go", "sub/deep/c.go", "sub/d.txt")

	got, err := globFiles(GlobInput{Pattern: "**/*.go", Path: dir})
	if err != nil {
		t.Fatalf("globFiles() error = %v", err)
	}

	for _, want := range []string{"a.go", "b.go", "c.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(got, "d.txt") {
		t.Errorf("output = %q, want the non-matching file excluded", got)
	}
}

func TestGlob_DefaultsToTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "here.go")
	t.Chdir(dir)

	got, err := globFiles(GlobInput{Pattern: "*.go"})
	if err != nil {
		t.Fatalf("globFiles() error = %v", err)
	}
	if !strings.Contains(got, "here.go") {
		t.Errorf("output = %q, want here.go", got)
	}
}

func TestGlob_ReportsNoMatchesExplicitly(t *testing.T) {
	// An empty result string is indistinguishable from a failed call.
	got, err := globFiles(GlobInput{Pattern: "*.nothing", Path: t.TempDir()})
	if err != nil {
		t.Fatalf("globFiles() error = %v", err)
	}
	if !strings.Contains(got, "No files") {
		t.Errorf("output = %q, want an explicit no-matches message", got)
	}
}

func TestGlob_RequiresAnAbsolutePathWhenGiven(t *testing.T) {
	_, err := globFiles(GlobInput{Pattern: "*.go", Path: "relative"})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("globFiles(relative path) error = %v, want an absolute-path requirement", err)
	}
}

func TestGlob_ExcludesDirectoriesFromResults(t *testing.T) {
	// A pattern like **/* matches directories too; listing them as results
	// would send the model to Read a directory.
	dir := t.TempDir()
	seed(t, dir, "sub/file.txt")

	got, err := globFiles(GlobInput{Pattern: "*", Path: dir})
	if err != nil {
		t.Fatalf("globFiles() error = %v", err)
	}
	if strings.Contains(got, filepath.Join(dir, "sub")+"\n") {
		t.Errorf("output = %q, want the directory itself excluded", got)
	}
}

func TestGlob_SchemaMarksOnlyPatternRequired(t *testing.T) {
	if got := requiredParams(t, newGlobTool()); len(got) != 1 || got[0] != "pattern" {
		t.Errorf("required = %v, want exactly [pattern]", got)
	}
}

// requiredParams returns the schema's required-parameter list for a tool.
//
// It marshals the ToolInfo, never info.ParamsOneOf: the latter's fields are
// unexported and marshal to "{}", so an assertion against it would pass
// vacuously no matter what the schema actually says.
func requiredParams(t *testing.T, tl tool.InvokableTool) []string {
	t.Helper()
	info, err := tl.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var parsed struct {
		JSONSchema struct {
			Required []string `json:"required"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return parsed.JSONSchema.Required
}
