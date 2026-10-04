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

func TestGrep_FindsMatchingLinesWithLineNumbers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("package main\nfunc target() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := grepFiles(GrepInput{Pattern: "target", Path: dir, OutputMode: "content", LineNumbers: true})
	if err != nil {
		t.Fatalf("grepFiles() error = %v", err)
	}
	if !strings.Contains(got, "target") || !strings.Contains(got, "2") {
		t.Errorf("output = %q, want the matching line and its number", got)
	}
}

func TestGrep_FilesWithMatchesListsPathsOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hit.go"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "miss.go"), []byte("nothing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := grepFiles(GrepInput{Pattern: "needle", Path: dir, OutputMode: "files_with_matches"})
	if err != nil {
		t.Fatalf("grepFiles() error = %v", err)
	}
	if !strings.Contains(got, "hit.go") {
		t.Errorf("output = %q, want hit.go", got)
	}
	if strings.Contains(got, "miss.go") {
		t.Errorf("output = %q, want miss.go excluded", got)
	}
	if strings.Contains(got, "needle") {
		t.Errorf("output = %q, want paths only, not matched content", got)
	}
}

func TestGrep_CaseInsensitiveFlag(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("NeEdLe\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := grepFiles(GrepInput{Pattern: "needle", Path: dir, OutputMode: "files_with_matches", IgnoreCase: true})
	if err != nil {
		t.Fatalf("grepFiles() error = %v", err)
	}
	if !strings.Contains(got, "a.txt") {
		t.Errorf("output = %q, want the case-insensitive match", got)
	}
}

func TestGrep_GlobNarrowsTheFileSet(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := grepFiles(GrepInput{Pattern: "needle", Path: dir, Glob: "*.go", OutputMode: "files_with_matches"})
	if err != nil {
		t.Fatalf("grepFiles() error = %v", err)
	}
	if !strings.Contains(got, "a.go") || strings.Contains(got, "a.txt") {
		t.Errorf("output = %q, want only the .go file", got)
	}
}

func TestGrep_CountModeReportsPerFileTotals(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hit\nmiss\nhit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := grepFiles(GrepInput{Pattern: "hit", Path: dir, OutputMode: "count"})
	if err != nil {
		t.Fatalf("grepFiles() error = %v", err)
	}
	if !strings.Contains(got, "2") {
		t.Errorf("output = %q, want a count of 2", got)
	}
}

func TestGrep_ReportsNoMatchesExplicitly(t *testing.T) {
	got, err := grepFiles(GrepInput{Pattern: "absent", Path: t.TempDir(), OutputMode: "content"})
	if err != nil {
		t.Fatalf("grepFiles() error = %v", err)
	}
	if !strings.Contains(got, "No matches") {
		t.Errorf("output = %q, want an explicit no-matches message", got)
	}
}

func TestGrep_RejectsAnInvalidRegex(t *testing.T) {
	// ripgrep would report this as a bare exit code, which is
	// indistinguishable from "no matches".
	_, err := grepFiles(GrepInput{Pattern: "([unclosed", Path: t.TempDir(), OutputMode: "content"})
	if err == nil {
		t.Error("grepFiles(invalid regex) = nil, want an error")
	}
}

func TestGrepGoFallback_ProducesPathLineTextLikeRipgrep(t *testing.T) {
	// The fallback runs whenever rg is absent, so its output has to be usable
	// the same way rather than merely non-empty. Exercised directly so this
	// holds on machines that do have rg installed.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nneedle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := grepWithGo(GrepInput{Pattern: "needle", Path: dir, OutputMode: "content", LineNumbers: true})
	if err != nil {
		t.Fatalf("grepWithGo() error = %v", err)
	}
	if !strings.Contains(got, "a.txt") || !strings.Contains(got, "2:") || !strings.Contains(got, "needle") {
		t.Errorf("output = %q, want path:line:text", got)
	}
}

func TestGrepGoFallback_SkipsGitAndNodeModules(t *testing.T) {
	// .git holds packed binary objects that match almost any pattern.
	dir := t.TempDir()
	seed(t, dir, ".git/objects/pack/thing", "node_modules/pkg/index.js", "real.txt")
	for _, f := range []string{".git/objects/pack/thing", "node_modules/pkg/index.js", "real.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := grepWithGo(GrepInput{Pattern: "needle", Path: dir, OutputMode: "files_with_matches"})
	if err != nil {
		t.Fatalf("grepWithGo() error = %v", err)
	}
	if !strings.Contains(got, "real.txt") {
		t.Errorf("output = %q, want real.txt", got)
	}
	if strings.Contains(got, ".git") || strings.Contains(got, "node_modules") {
		t.Errorf("output = %q, want .git and node_modules skipped", got)
	}
}

func TestGrep_SchemaKeepsClaudeFlagNamesAndOptionalFields(t *testing.T) {
	// The hyphenated names match Claude's parameter names so a model trained
	// on them emits the right JSON. Only the exact tag "-" means "skip", so
	// json:"-i" is legal — verified against eino v0.9.20, kept so a
	// dependency bump cannot break it silently.
	info, err := newGrepTool().Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var parsed struct {
		JSONSchema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	for _, want := range []string{"pattern", "path", "glob", "output_mode", "-i", "-n"} {
		if _, ok := parsed.JSONSchema.Properties[want]; !ok {
			t.Errorf("schema is missing the %q parameter; got %v", want, parsed.JSONSchema.Properties)
		}
	}

	if got := requiredParams(t, newGrepTool()); len(got) != 1 || got[0] != "pattern" {
		t.Errorf("required = %v, want exactly [pattern]", got)
	}
}

func TestGrep_DescriptionsSurviveTagParsing(t *testing.T) {
	// A comma inside jsonschema:"description=..." truncates it there. This
	// caught a real defect: "One of content, files_with_matches, or count"
	// reached the model as "One of content".
	info, err := newGrepTool().Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var parsed struct {
		JSONSchema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	desc := parsed.JSONSchema.Properties["output_mode"].Description
	for _, want := range []string{"content", "files_with_matches", "count"} {
		if !strings.Contains(desc, want) {
			t.Errorf("output_mode description = %q, want it to name %q — a comma in the tag truncates it", desc, want)
		}
	}
}
