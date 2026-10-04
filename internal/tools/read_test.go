package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjkondratuk/dewey/internal/files"
)

func TestRead_ReturnsLineNumberedContent(t *testing.T) {
	// Skills and agents cite line numbers from Read's output, so the numbering
	// is part of the contract rather than decoration.
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readFile(files.NewTracker(), ReadInput{FilePath: path})
	if err != nil {
		t.Fatalf("readFile() error = %v", err)
	}

	for _, want := range []string{"     1\talpha", "     2\tbeta", "     3\tgamma"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, got)
		}
	}
}

func TestRead_HonorsOffsetAndLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readFile(files.NewTracker(), ReadInput{FilePath: path, Offset: 2, Limit: 2})
	if err != nil {
		t.Fatalf("readFile() error = %v", err)
	}

	if !strings.Contains(got, "     2\ttwo") || !strings.Contains(got, "     3\tthree") {
		t.Errorf("output = %q, want lines 2 and 3", got)
	}
	if strings.Contains(got, "one") || strings.Contains(got, "four") {
		t.Errorf("output = %q, want lines 1 and 4 excluded", got)
	}
}

func TestRead_RecordsTheReadSoModificationIsAllowed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("contents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracker := files.NewTracker()

	if err := tracker.CheckWritable(path); err == nil {
		t.Fatal("CheckWritable() = nil before any read, want an error")
	}
	if _, err := readFile(tracker, ReadInput{FilePath: path}); err != nil {
		t.Fatalf("readFile() error = %v", err)
	}
	if err := tracker.CheckWritable(path); err != nil {
		t.Errorf("CheckWritable() after read = %v, want nil", err)
	}
}

func TestRead_RequiresAnAbsolutePath(t *testing.T) {
	// A relative path resolves against dewey's working directory, which is not
	// necessarily what the model meant.
	_, err := readFile(files.NewTracker(), ReadInput{FilePath: "relative.txt"})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("readFile(relative) error = %v, want it to require an absolute path", err)
	}
}

func TestRead_ReportsAnEmptyFileRatherThanReturningNothing(t *testing.T) {
	// A bare empty string reads as a failed call; the model then retries or
	// reports the file as unreadable.
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readFile(files.NewTracker(), ReadInput{FilePath: path})
	if err != nil {
		t.Fatalf("readFile() error = %v", err)
	}
	if !strings.Contains(got, "empty") {
		t.Errorf("output = %q, want it to say the file is empty", got)
	}
}

func TestRead_RejectsADirectory(t *testing.T) {
	dir := t.TempDir()

	_, err := readFile(files.NewTracker(), ReadInput{FilePath: dir})
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("readFile(directory) error = %v, want it to say the path is a directory", err)
	}
}

func TestRead_ReportsAnOffsetPastTheEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readFile(files.NewTracker(), ReadInput{FilePath: path, Offset: 99})
	if err != nil {
		t.Fatalf("readFile() error = %v", err)
	}
	if !strings.Contains(got, "past the end") {
		t.Errorf("output = %q, want it to say the offset is past the end", got)
	}
}

func TestRead_SchemaMarksOnlyFilePathRequired(t *testing.T) {
	// A field is optional ONLY because its json tag carries ",omitempty" —
	// jsonschema:"required" does not control this. Without omitempty the model
	// is forced to supply offset and limit on every single call.
	info, err := newReadTool(files.NewTracker()).Info(context.Background())
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
	if len(parsed.JSONSchema.Required) != 1 || parsed.JSONSchema.Required[0] != "file_path" {
		t.Errorf("required = %v, want exactly [file_path]", parsed.JSONSchema.Required)
	}
}
