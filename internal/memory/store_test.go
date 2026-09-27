package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestFileStore_SaveRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "created-on-demand")
	s := NewFileStore(dir)

	want := Memory{
		ID:      "uses-mise",
		Text:    "Pins Go with mise; the system toolchain is wrong.",
		Org:     "acme",
		Tags:    []string{"go"},
		Created: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC),
	}
	if err := s.Save(context.Background(), want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	res, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Memories) != 1 {
		t.Fatalf("loaded %d memories, want 1", len(res.Memories))
	}
	got := res.Memories[0]
	if got.ID != want.ID || got.Text != want.Text || got.Org != want.Org {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "go" {
		t.Errorf("Tags = %v, want [go]", got.Tags)
	}
	if !got.Created.Equal(want.Created) {
		t.Errorf("Created = %v, want %v", got.Created, want.Created)
	}
}

func TestFileStore_SaveRejectsAnUnsafeID(t *testing.T) {
	s := NewFileStore(t.TempDir())
	err := s.Save(context.Background(), Memory{ID: "../escape", Text: "nope"})
	if err == nil {
		t.Fatal("expected an error for an id that escapes the memory directory")
	}
}

func TestFileStore_RevisionChangesOnSave(t *testing.T) {
	s := NewFileStore(t.TempDir())
	before := s.Revision(context.Background())

	if err := s.Save(context.Background(), Memory{ID: "x", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if after := s.Revision(context.Background()); after == before {
		t.Errorf("Revision unchanged after Save (%q) — cached memories would go stale", after)
	}
}
