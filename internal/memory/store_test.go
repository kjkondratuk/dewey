package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// Save used to interpolate Org/Tags raw into YAML, so these inputs produced
// files it accepted and Load could then never read again — the user was told
// the memory was saved and it was permanently unreadable.
func TestFileStore_SaveSurvivesHostileFrontmatterValues(t *testing.T) {
	cases := []struct {
		name string
		mem  Memory
	}{
		{"newline in org", Memory{ID: "inject-org", Org: "acme\nid: hijacked", Text: "Body."}},
		{"bracket in tag", Memory{ID: "inject-tag", Tags: []string{"go]\nid: hijacked"}, Text: "Body."}},
		{"hash in tag", Memory{ID: "hash-tag", Tags: []string{"go #x", "b"}, Text: "Body."}},
		{"colon in org", Memory{ID: "colon-org", Org: "acme: inc", Text: "Body."}},
		{"quote in text", Memory{ID: "quote-text", Text: `He said "---" and left.`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewFileStore(t.TempDir())
			if err := s.Save(context.Background(), c.mem); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			res, err := s.Load(context.Background())
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if len(res.Skipped) != 0 {
				t.Errorf("Load reported %+v — Save wrote a file it cannot read", res.Skipped)
			}
			if len(res.Memories) != 1 {
				t.Fatalf("loaded %d memories, want 1", len(res.Memories))
			}
			if got := res.Memories[0].Text; got != strings.TrimSpace(c.mem.Text) {
				t.Errorf("Text = %q, want %q", got, c.mem.Text)
			}
		})
	}
}

func TestFileStore_SaveRejectsEmptyText(t *testing.T) {
	s := NewFileStore(t.TempDir())
	for _, text := range []string{"", "   ", "\n\t "} {
		if err := s.Save(context.Background(), Memory{ID: "empty", Text: text}); err == nil {
			t.Errorf("Save(text=%q) succeeded; Load would then reject the file forever", text)
		}
	}
}

// Load must never observe a half-written file. Before writes went through a
// rename, a concurrent save/load loop produced hundreds of torn reads and a
// stream of skip notices about a corpus that was actually fine.
func TestFileStore_ConcurrentSaveAndLoadNeverSeesATornFile(t *testing.T) {
	s := NewFileStore(t.TempDir())
	long := Memory{ID: "churn", Text: strings.Repeat("a long body ", 400)}
	short := Memory{ID: "churn", Text: "tiny"}
	if err := s.Save(context.Background(), long); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			m := long
			if i%2 == 0 {
				m = short
			}
			if err := s.Save(context.Background(), m); err != nil {
				t.Errorf("Save() error = %v", err)
				return
			}
		}
	}()

	torn := 0
	for i := 0; i < 400; i++ {
		res, err := s.Load(context.Background())
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if len(res.Skipped) > 0 {
			torn++
		}
		for _, m := range res.Memories {
			if m.Text != strings.TrimSpace(long.Text) && m.Text != short.Text {
				torn++
			}
		}
	}
	<-done

	if torn != 0 {
		t.Errorf("observed %d torn or unreadable reads during concurrent saves", torn)
	}
}

func TestFileStore_ConcurrentSavesDoNotCorrupt(t *testing.T) {
	s := NewFileStore(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Save(context.Background(), Memory{ID: "shared", Text: fmt.Sprintf("writer %d", i)}); err != nil {
				t.Errorf("Save() error = %v", err)
			}
		}(i)
	}
	wg.Wait()

	res, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Skipped) != 0 || len(res.Memories) != 1 {
		t.Errorf("after concurrent saves: memories=%d skipped=%+v, want exactly 1 readable memory", len(res.Memories), res.Skipped)
	}
}

func TestFileStore_SaveLeavesNoTempFilesBehind(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	if err := s.Save(context.Background(), Memory{ID: "ok", Text: "Body."}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file %q left behind", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want 1", len(entries))
	}
}

// The in-place-edit test changes body length, so size alone carries it and a
// digest that ignored mtime would still pass. This pins mtime specifically.
func TestFileStore_RevisionNoticesASameLengthEdit(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "x.md", "---\nid: x\n---\nAAAA.\n")
	s := NewFileStore(dir)
	before := s.Revision(context.Background())

	writeMemFile(t, dir, "x.md", "---\nid: x\n---\nBBBB.\n")
	// Force a distinct mtime: the write may land inside the same timestamp tick.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, "x.md"), future, future); err != nil {
		t.Fatal(err)
	}

	if after := s.Revision(context.Background()); after == before {
		t.Error("Revision ignored a same-length edit — the digest is not covering mtime")
	}
}

// A directory named *.md must not be read as a memory.
func TestFileStore_IgnoresDirectoriesNamedLikeMemories(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "notamemory.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMemFile(t, dir, "real.md", "---\nid: real\n---\nReal.\n")

	res, err := NewFileStore(dir).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Memories) != 1 || res.Memories[0].ID != "real" {
		t.Errorf("memories = %+v, want only the actual file", res.Memories)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("Skipped = %+v, want a directory to be ignored silently", res.Skipped)
	}
}

// The id becomes a filename and can come from model output, so this is a
// containment check. One input asserting only err != nil would also pass if
// Save rejected everything, so this walks the parent afterwards.
func TestFileStore_SaveRejectsEveryUnsafeIDAndWritesNothingOutside(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "memory")
	s := NewFileStore(dir)

	hostile := []string{
		"../escape", "a/b", "..", ".", "", "ok\n../escape", "../escape\nok",
		"ok-id\x00", "A-B", "a_b", "a.b", "/abs", "a//b", "-lead", "trail-",
	}
	for _, id := range hostile {
		if err := s.Save(context.Background(), Memory{ID: id, Text: "Body."}); err == nil {
			t.Errorf("Save(id=%q) was accepted, want rejected", id)
		}
	}

	// A legitimate id must still work, or the test would pass by rejecting all.
	if err := s.Save(context.Background(), Memory{ID: "legitimate-id", Text: "Body."}); err != nil {
		t.Fatalf("Save of a valid id failed: %v", err)
	}

	var outside []string
	err := filepath.WalkDir(parent, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Dir(path) != dir {
			outside = append(outside, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outside) != 0 {
		t.Errorf("files written outside the memory directory: %v", outside)
	}
}

// The "no body text" rule is reachable three ways and was asserted none.
func TestFileStore_BodylessMemoriesAreReportedNotLoaded(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "frontmatter-only.md", "---\nid: a\n---\n")
	writeMemFile(t, dir, "whitespace-body.md", "---\nid: b\n---\n   \n\t\n")

	res, err := NewFileStore(dir).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Memories) != 0 {
		t.Errorf("memories = %+v, want none — neither file has body text", res.Memories)
	}
	if len(res.Skipped) != 2 {
		t.Errorf("Skipped = %+v, want both files reported", res.Skipped)
	}
}

func TestFileStore_CreatedIsParsedFromHandWrittenFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "dated.md", "---\nid: dated\ncreated: 2026-09-27\n---\nBody.\n")

	res, _ := NewFileStore(dir).Load(context.Background())
	if len(res.Memories) != 1 {
		t.Fatalf("loaded %d memories, want 1", len(res.Memories))
	}
	want := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	if got := res.Memories[0].Created; !got.Equal(want) {
		t.Errorf("Created = %v, want %v", got, want)
	}
}

// A project memory directory is meant to be committed, so a cloned repo can
// contain a symlink — and these files are read into the system message, the
// highest-authority slot. A link to a private key would put key material in
// the model's context.
func TestFileStore_RefusesSymlinksAndReportsThem(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("---\nid: s\n---\nSUPERSECRETKEYMATERIAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "note.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeMemFile(t, dir, "real.md", "---\nid: real\n---\nReal.\n")

	res, err := NewFileStore(dir).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for _, m := range res.Memories {
		if strings.Contains(m.Text, "SUPERSECRETKEYMATERIAL") {
			t.Fatal("followed a symlink: linked file contents would reach the system prompt")
		}
	}
	if len(res.Memories) != 1 || res.Memories[0].ID != "real" {
		t.Errorf("memories = %+v, want only the regular file", res.Memories)
	}
	if len(res.Skipped) != 1 {
		t.Errorf("Skipped = %+v, want the symlink reported rather than ignored", res.Skipped)
	}
}

// A silently ignored memory is a preference the user believes is in effect.
func TestFileStore_MatchesTheExtensionCaseInsensitively(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "shouty.MD", "---\nid: shouty\n---\nStill a memory.\n")

	res, err := NewFileStore(dir).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Memories) != 1 {
		t.Errorf("memories = %+v, want the .MD file loaded", res.Memories)
	}
}
