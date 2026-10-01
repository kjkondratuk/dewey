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

// A "----" rule is a normal way to open a body, and the fence above it has
// already done its job by then. This one is a guard rather than a regression:
// it held before the terminator was tightened too.
func TestFileStore_BodyMayOpenWithAHorizontalRule(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "ruled.md", "---\nid: ruled\ntags: [go]\n---\n----\nPrefers ASCII rules.\n")

	res, _ := NewFileStore(dir).Load(context.Background())
	if len(res.Memories) != 1 {
		t.Fatalf("loaded %d memories, want 1 (skipped: %+v)", len(res.Memories), res.Skipped)
	}
	m := res.Memories[0]
	if len(m.Tags) != 1 || m.Tags[0] != "go" {
		t.Errorf("Tags = %v, want [go]", m.Tags)
	}
	if m.Text != "----\nPrefers ASCII rules." {
		t.Errorf("Text = %q, want the body including its leading rule", m.Text)
	}
}

// A fence typed with four dashes never closes the frontmatter. Matching any
// line that merely starts with "---" turned that typo into a memory whose text
// carried the leftover dash, so the user got a subtly corrupted preference and
// no indication anything was wrong. Refusing the file surfaces it instead.
func TestFileStore_UnclosedFrontmatterIsReportedNotMangled(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "typo.md", "---\nid: typo\ntags: [go]\n----\nPins Go with mise.\n")

	res, err := NewFileStore(dir).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Memories) != 0 {
		t.Errorf("Memories = %+v, want none — the frontmatter is never closed", res.Memories)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].File != "typo.md" {
		t.Fatalf("Skipped = %+v, want typo.md reported", res.Skipped)
	}
}

// The frontmatter fence is what gates a memory to an organization. Any shape
// the parser fails to recognize turns the whole file into body prose with an
// empty Org, which quietly promotes a scoped memory into a universal one — and
// pastes the raw YAML into the system prompt as well. Each of these is a shape
// a real editor produces.
func TestFileStore_RecognizesFrontmatterInEveryRealisticFenceShape(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"crlf", "---\r\nid: scoped\r\norg: acme\r\ntags: [go]\r\n---\r\nAcme only.\r\n"},
		{"utf8 bom", "\ufeff---\nid: scoped\norg: acme\ntags: [go]\n---\nAcme only.\n"},
		{"trailing space after the opening fence", "--- \nid: scoped\norg: acme\ntags: [go]\n---\nAcme only.\n"},
		{"trailing space after the closing fence", "---\nid: scoped\norg: acme\ntags: [go]\n--- \nAcme only.\n"},
		{"leading blank line", "\n---\nid: scoped\norg: acme\ntags: [go]\n---\nAcme only.\n"},
		{"crlf with a bom", "\ufeff---\r\nid: scoped\r\norg: acme\r\ntags: [go]\r\n---\r\nAcme only.\r\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeMemFile(t, dir, "scoped.md", c.raw)

			res, err := NewFileStore(dir).Load(context.Background())
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if len(res.Memories) != 1 {
				t.Fatalf("loaded %d memories, want 1 (skipped: %+v)", len(res.Memories), res.Skipped)
			}
			m := res.Memories[0]
			if m.Org != "acme" {
				t.Errorf("Org = %q, want acme — an unrecognized fence makes a scoped memory apply everywhere", m.Org)
			}
			if len(m.Tags) != 1 || m.Tags[0] != "go" {
				t.Errorf("Tags = %v, want [go]", m.Tags)
			}
			if m.Text != "Acme only." {
				t.Errorf("Text = %q, want just the body — the frontmatter must not leak into the prompt", m.Text)
			}
		})
	}
}

// A file with no frontmatter at all is a valid shape: the id comes from the
// filename and the memory applies everywhere. Fence leniency must not start
// reading the first prose line as YAML.
func TestFileStore_FileWithoutFrontmatterLoadsAsBodyOnly(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "plain-prose.md", "Writes commit bodies that explain why.\n")

	res, err := NewFileStore(dir).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Memories) != 1 {
		t.Fatalf("loaded %d memories, want 1 (skipped: %+v)", len(res.Memories), res.Skipped)
	}
	m := res.Memories[0]
	if m.ID != "plain-prose" || m.Text != "Writes commit bodies that explain why." {
		t.Errorf("memory = %+v, want the filename as id and the whole file as text", m)
	}
}

// An empty frontmatter block is how a user says "this memory is universal".
// Searching for the terminator inside the text after the opening fence missed a
// terminator sitting on that text's first line, so this shape was reported as
// never closed and the memory was dropped.
func TestFileStore_EmptyFrontmatterBlockIsAccepted(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "universal.md", "---\n---\nApplies everywhere.\n")

	res, err := NewFileStore(dir).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Memories) != 1 {
		t.Fatalf("loaded %d memories, want 1 (skipped: %+v)", len(res.Memories), res.Skipped)
	}
	m := res.Memories[0]
	if m.ID != "universal" || m.Text != "Applies everywhere." || m.Org != "" {
		t.Errorf("memory = %+v, want id from the filename, the body, and no org gate", m)
	}
}

// Org and tags are written by hand as often as by the agent, and the detected
// context is lowercase. Comparing them raw fails closed — the memory simply
// never applies — which is silent and impossible to debug from the UI.
func TestFileStore_OrgAndTagsAreNormalizedOnLoad(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "cased.md", "---\nid: cased\norg: \"  Acme  \"\ntags: [Go, \" Testing \"]\n---\nCased.\n")

	res, _ := NewFileStore(dir).Load(context.Background())
	if len(res.Memories) != 1 {
		t.Fatalf("loaded %d memories, want 1 (skipped: %+v)", len(res.Memories), res.Skipped)
	}
	m := res.Memories[0]
	if m.Org != "acme" {
		t.Errorf("Org = %q, want acme — DetectContext yields lowercase orgs", m.Org)
	}
	if len(m.Tags) != 2 || m.Tags[0] != "go" || m.Tags[1] != "testing" {
		t.Errorf("Tags = %v, want [go testing]", m.Tags)
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

// Editing a memory in place is the most likely way a user curates one, and a
// directory's own mtime does not move when an existing file is overwritten —
// so a directory-level change token would make the edit invisible until
// restart.
func TestFileStore_RevisionChangesWhenAFileIsEditedInPlace(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "x.md", "---\nid: x\n---\nOriginal.\n")
	s := NewFileStore(dir)
	before := s.Revision(context.Background())

	writeMemFile(t, dir, "x.md", "---\nid: x\n---\nRewritten, with a different length.\n")

	if after := s.Revision(context.Background()); after == before {
		t.Error("Revision unchanged after an in-place edit — the edit would not be picked up until restart")
	}
}

func TestFileStore_RevisionIsStableWithoutChanges(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "x.md", "---\nid: x\n---\nStable.\n")
	s := NewFileStore(dir)

	first := s.Revision(context.Background())
	if second := s.Revision(context.Background()); second != first {
		t.Errorf("Revision changed without any edit (%q then %q) — the cache would reload on every call", first, second)
	}
}
