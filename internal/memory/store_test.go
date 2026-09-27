package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeMemFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFileStore_LoadParsesFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "prefer-table-tests.md", `---
id: prefer-table-tests
created: 2026-09-27
org: acme
tags: [go, testing]
---
Prefers table-driven tests.
`)

	res, err := NewFileStore(dir).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Memories) != 1 {
		t.Fatalf("loaded %d memories, want 1", len(res.Memories))
	}

	m := res.Memories[0]
	if m.ID != "prefer-table-tests" {
		t.Errorf("ID = %q", m.ID)
	}
	if m.Org != "acme" {
		t.Errorf("Org = %q, want acme", m.Org)
	}
	if len(m.Tags) != 2 || m.Tags[0] != "go" || m.Tags[1] != "testing" {
		t.Errorf("Tags = %v, want [go testing]", m.Tags)
	}
	if m.Text != "Prefers table-driven tests." {
		t.Errorf("Text = %q, want the body with surrounding whitespace trimmed", m.Text)
	}
}

func TestFileStore_MissingDirectoryIsEmptyNotAnError(t *testing.T) {
	res, err := NewFileStore(filepath.Join(t.TempDir(), "nope")).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v, want nil for a missing directory", err)
	}
	if len(res.Memories) != 0 {
		t.Errorf("loaded %d memories, want 0", len(res.Memories))
	}
}

// A single unparseable file must not cost the user their whole profile.
func TestFileStore_MalformedFileIsSkippedAndReported(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "good.md", "---\nid: good\n---\nFine.\n")
	writeMemFile(t, dir, "bad.md", "---\nid: [unclosed\n---\nBroken.\n")

	res, err := NewFileStore(dir).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Memories) != 1 || res.Memories[0].ID != "good" {
		t.Errorf("Memories = %+v, want just the good one", res.Memories)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("Skipped = %+v, want exactly one notice", res.Skipped)
	}
	if res.Skipped[0].File != "bad.md" {
		t.Errorf("Skipped file = %q, want bad.md", res.Skipped[0].File)
	}
}

func TestFileStore_MissingIDFallsBackToFilename(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "uses-mise.md", "---\ntags: [go]\n---\nUses mise.\n")

	res, _ := NewFileStore(dir).Load(context.Background())
	if len(res.Memories) != 1 || res.Memories[0].ID != "uses-mise" {
		t.Errorf("Memories = %+v, want ID derived from the filename", res.Memories)
	}
}

func TestFileStore_IgnoresNonMarkdownFiles(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "notes.txt", "not a memory")
	writeMemFile(t, dir, "real.md", "---\nid: real\n---\nReal.\n")

	res, _ := NewFileStore(dir).Load(context.Background())
	if len(res.Memories) != 1 || res.Memories[0].ID != "real" {
		t.Errorf("Memories = %+v, want only the .md file", res.Memories)
	}
}
