package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckWritable_NewFileNeedsNoPriorRead(t *testing.T) {
	tr := NewTracker()
	path := filepath.Join(t.TempDir(), "new.txt")

	if err := tr.CheckWritable(path); err != nil {
		t.Errorf("CheckWritable(new file) = %v, want nil", err)
	}
}

func TestCheckWritable_ExistingFileRequiresAPriorRead(t *testing.T) {
	tr := NewTracker()
	path := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(path, []byte("contents"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := tr.CheckWritable(path)
	if err == nil {
		t.Fatal("CheckWritable(unread existing file) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "has not been read") {
		t.Errorf("error = %q, want it to say the file was not read", err)
	}
}

func TestCheckWritable_PassesAfterARead(t *testing.T) {
	tr := NewTracker()
	path := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(path, []byte("contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	tr.MarkRead(path, info.ModTime())

	if err := tr.CheckWritable(path); err != nil {
		t.Errorf("CheckWritable(after read) = %v, want nil", err)
	}
}

func TestCheckWritable_RejectsAFileChangedSinceTheRead(t *testing.T) {
	// Writing based on contents that have since changed on disk silently
	// discards whatever changed them.
	tr := NewTracker()
	path := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	tr.MarkRead(path, info.ModTime())

	// Rewrite with an explicitly later mtime; filesystem timestamp
	// granularity makes a bare rewrite unreliable within one test.
	if err := os.WriteFile(path, []byte("changed underneath"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}

	err = tr.CheckWritable(path)
	if err == nil {
		t.Fatal("CheckWritable(changed file) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "changed on disk") {
		t.Errorf("error = %q, want it to say the file changed", err)
	}
}

func TestTracker_NormalizesPathSpelling(t *testing.T) {
	// A file read as ./x and written as /abs/x is one file.
	tr := NewTracker()
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(path, []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	tr.MarkRead(filepath.Join(dir, ".", "x.txt"), info.ModTime())

	if _, ok := tr.ReadAt(path); !ok {
		t.Error("ReadAt(clean path) = not found, want the entry recorded under the messy spelling")
	}
}
