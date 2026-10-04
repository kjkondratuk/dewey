package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"

	"github.com/kjkondratuk/dewey/internal/files"
)

// readThen reads path through the tracker so a modification is permitted,
// mirroring what the model does in a real session.
func readThen(t *testing.T, tracker *files.Tracker, path string) {
	t.Helper()
	if _, err := readFile(tracker, ReadInput{FilePath: path}); err != nil {
		t.Fatalf("readFile(%s) error = %v", path, err)
	}
}

func TestWrite_CreatesANewFile(t *testing.T) {
	tracker := files.NewTracker()
	path := filepath.Join(t.TempDir(), "new.txt")

	if _, err := writeFile(tracker, WriteInput{FilePath: path, Content: "hello\n"}); err != nil {
		t.Fatalf("writeFile() error = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello\n" {
		t.Errorf("file contents = %q, want %q", got, "hello\n")
	}
}

func TestWrite_RefusesToClobberAnUnreadFile(t *testing.T) {
	tracker := files.NewTracker()
	path := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(path, []byte("precious\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := writeFile(tracker, WriteInput{FilePath: path, Content: "replacement\n"})
	if err == nil {
		t.Fatal("writeFile() = nil, want a refusal")
	}

	got, _ := os.ReadFile(path)
	if string(got) != "precious\n" {
		t.Errorf("file contents = %q, want the original preserved", got)
	}
}

func TestWrite_AllowsOverwriteAfterARead(t *testing.T) {
	tracker := files.NewTracker()
	path := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	readThen(t, tracker, path)

	if _, err := writeFile(tracker, WriteInput{FilePath: path, Content: "new\n"}); err != nil {
		t.Fatalf("writeFile() error = %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new\n" {
		t.Errorf("file contents = %q, want %q", got, "new\n")
	}
}

func TestWrite_LeavesTheFileModifiableAgain(t *testing.T) {
	// A write updates the mtime, so without re-recording it a second write or
	// a following Edit would be rejected as "changed on disk".
	tracker := files.NewTracker()
	path := filepath.Join(t.TempDir(), "f.txt")

	if _, err := writeFile(tracker, WriteInput{FilePath: path, Content: "first\n"}); err != nil {
		t.Fatalf("first writeFile() error = %v", err)
	}
	if _, err := writeFile(tracker, WriteInput{FilePath: path, Content: "second\n"}); err != nil {
		t.Errorf("second writeFile() error = %v, want nil", err)
	}
}

func TestWrite_PreservesTheExistingFileMode(t *testing.T) {
	tracker := files.NewTracker()
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	readThen(t, tracker, path)

	if _, err := writeFile(tracker, WriteInput{FilePath: path, Content: "#!/bin/sh\necho hi\n"}); err != nil {
		t.Fatalf("writeFile() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755 preserved — rewriting a script must not strip its exec bit", info.Mode().Perm())
	}
}

func TestWrite_RequiresAnAbsolutePath(t *testing.T) {
	_, err := writeFile(files.NewTracker(), WriteInput{FilePath: "relative.txt", Content: "x"})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("writeFile(relative) error = %v, want an absolute-path requirement", err)
	}
}

func TestEdit_ReplacesAUniqueMatch(t *testing.T) {
	tracker := files.NewTracker()
	path := filepath.Join(t.TempDir(), "f.go")
	if err := os.WriteFile(path, []byte("func old() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	readThen(t, tracker, path)

	if _, err := editFile(tracker, EditInput{FilePath: path, OldString: "old", NewString: "renamed"}); err != nil {
		t.Fatalf("editFile() error = %v", err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != "func renamed() {}\n" {
		t.Errorf("file contents = %q, want the replacement applied", got)
	}
}

func TestEdit_RefusesANonUniqueMatch(t *testing.T) {
	// This is the invariant TDD and refactoring skills depend on. Editing the
	// first of several matches silently changes the wrong line.
	tracker := files.NewTracker()
	path := filepath.Join(t.TempDir(), "f.go")
	if err := os.WriteFile(path, []byte("x := 1\ny := 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	readThen(t, tracker, path)

	_, err := editFile(tracker, EditInput{FilePath: path, OldString: "1", NewString: "2"})
	if err == nil {
		t.Fatal("editFile() = nil, want a refusal for an ambiguous match")
	}
	if !strings.Contains(err.Error(), "2 times") {
		t.Errorf("error = %q, want it to report the match count", err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != "x := 1\ny := 1\n" {
		t.Errorf("file contents = %q, want the file untouched", got)
	}
}

func TestEdit_ReplaceAllAcceptsMultipleMatches(t *testing.T) {
	tracker := files.NewTracker()
	path := filepath.Join(t.TempDir(), "f.go")
	if err := os.WriteFile(path, []byte("a := 1\nb := 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	readThen(t, tracker, path)

	if _, err := editFile(tracker, EditInput{FilePath: path, OldString: "1", NewString: "2", ReplaceAll: true}); err != nil {
		t.Fatalf("editFile() error = %v", err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != "a := 2\nb := 2\n" {
		t.Errorf("file contents = %q, want both replaced", got)
	}
}

func TestEdit_ReportsAMissingMatch(t *testing.T) {
	tracker := files.NewTracker()
	path := filepath.Join(t.TempDir(), "f.go")
	if err := os.WriteFile(path, []byte("contents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	readThen(t, tracker, path)

	_, err := editFile(tracker, EditInput{FilePath: path, OldString: "absent", NewString: "x"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("editFile() error = %v, want a not-found error", err)
	}
}

func TestEdit_RefusesAnUnreadFile(t *testing.T) {
	tracker := files.NewTracker()
	path := filepath.Join(t.TempDir(), "f.go")
	if err := os.WriteFile(path, []byte("contents\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := editFile(tracker, EditInput{FilePath: path, OldString: "contents", NewString: "x"})
	if err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Errorf("editFile() error = %v, want a read-first refusal", err)
	}
}

func TestEdit_RejectsAnIdenticalReplacement(t *testing.T) {
	// A no-op edit means the model has lost track of the file's state; saying
	// so is more useful than reporting success.
	tracker := files.NewTracker()
	path := filepath.Join(t.TempDir(), "f.go")
	if err := os.WriteFile(path, []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	readThen(t, tracker, path)

	_, err := editFile(tracker, EditInput{FilePath: path, OldString: "same", NewString: "same"})
	if err == nil || !strings.Contains(err.Error(), "identical") {
		t.Errorf("editFile() error = %v, want an identical-strings error", err)
	}
}

func TestEdit_LeavesTheFileEditableAgain(t *testing.T) {
	// Sequential edits are the normal case; the second must not trip the
	// changed-on-disk check caused by the first.
	tracker := files.NewTracker()
	path := filepath.Join(t.TempDir(), "f.go")
	if err := os.WriteFile(path, []byte("one two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	readThen(t, tracker, path)

	if _, err := editFile(tracker, EditInput{FilePath: path, OldString: "one", NewString: "1"}); err != nil {
		t.Fatalf("first editFile() error = %v", err)
	}
	if _, err := editFile(tracker, EditInput{FilePath: path, OldString: "two", NewString: "2"}); err != nil {
		t.Errorf("second editFile() error = %v, want nil", err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != "1 2\n" {
		t.Errorf("file contents = %q, want both edits applied", got)
	}
}

func TestEdit_PreservesTheFileMode(t *testing.T) {
	tracker := files.NewTracker()
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte("echo old\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	readThen(t, tracker, path)

	if _, err := editFile(tracker, EditInput{FilePath: path, OldString: "old", NewString: "new"}); err != nil {
		t.Fatalf("editFile() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755 preserved", info.Mode().Perm())
	}
}

func TestModify_SchemasMarkOptionalFieldsOptional(t *testing.T) {
	// A field is optional ONLY because its json tag carries ",omitempty" —
	// jsonschema:"required" does not control it. Without omitempty the model
	// must supply replace_all on every Edit call.
	//
	// Marshal the ToolInfo, never info.ParamsOneOf: the latter's fields are
	// unexported and marshal to "{}", so the assertion would pass vacuously.
	required := func(t *testing.T, tl tool.InvokableTool) []string {
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

	tracker := files.NewTracker()

	gotWrite := required(t, newWriteTool(tracker))
	if len(gotWrite) != 2 {
		t.Errorf("Write required = %v, want exactly [file_path content]", gotWrite)
	}

	gotEdit := required(t, newEditTool(tracker))
	for _, name := range gotEdit {
		if name == "replace_all" {
			t.Errorf("Edit required = %v, want replace_all optional", gotEdit)
		}
	}
	if len(gotEdit) != 3 {
		t.Errorf("Edit required = %v, want exactly [file_path old_string new_string]", gotEdit)
	}
}
