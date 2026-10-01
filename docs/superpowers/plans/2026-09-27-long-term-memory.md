# Long-term Memory Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give dewey a persistent, user-curated profile — preferences and durable facts — that loads at launch, is scoped by organization and stack, and is only ever written with the user's approval.

**Architecture:** Memories are markdown files with YAML frontmatter, read from a global store (`~/.dewey/memory`) and a project store (`<project>/.dewey/memory`) and merged project-over-global. A retriever filters them against a detected context (org gate + topical tags). A provider caches the result and reassembles a system-prompt block whenever the underlying stores change. The agent holds the system message as a field — never in `history` — so compaction cannot consume it.

**Tech Stack:** Go 1.26.2, `gopkg.in/yaml.v3` (already a dependency), `cloudwego/eino` schema/tool types, `pkoukk/tiktoken-go` via the existing `internal/compaction` estimator.

**Design:** `docs/superpowers/specs/2026-09-27-long-term-memory-design.md`

**Build note:** this machine's Go toolchain needs an explicit prefix on every command:

```bash
export GOTOOLCHAIN=local && export GOROOT=/Users/kkondratuk/.local/share/mise/installs/go/1.26.2 && export PATH="$GOROOT/bin:$PATH"
```

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/memory/memory.go` | `Memory` and `Context` types. No behavior. |
| `internal/memory/store.go` | `Store` interface, `LoadResult`, `FileStore` (one directory). |
| `internal/memory/merged.go` | `MergedStore` — composes global + project. |
| `internal/memory/detect.go` | `DetectContext` — stack markers and org resolution. |
| `internal/memory/retriever.go` | `Retriever` interface (the seam) and `TagRetriever`. |
| `internal/memory/provider.go` | `Provider` — caching, budget, block assembly. |
| `internal/memory/recorder.go` | `Recorder` — stamps org/id and saves. Backs the tool. |
| `internal/compaction/tokens.go` | Gains `EstimateText`, reusing the cached encoder. |
| `internal/config/config.go` | `Memory` config section and project-level `org`. |
| `prompts/system_base.md` | Base agent instructions, embedded. |
| `prompts/prompts.go` | Exports `SystemBase`. |
| `internal/agent/openai.go` | System message as a field; prepended per `Stream`. |
| `internal/tools/tool_factory.go` | Registers `remember` when a recorder is supplied. |
| `main.go` | Wiring. |

Tasks 1–7 are pure `internal/memory` and `internal/config` work with no dependency on the agent. Task 8 (system prompt) and Task 9 (agent) are the risky ones and are deliberately separated so each is independently testable.

---

### Task 1: Memory types and single-directory store (load)

**Files:**
- Create: `internal/memory/memory.go`
- Create: `internal/memory/store.go`
- Test: `internal/memory/store_test.go`

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory -run TestFileStore -v`
Expected: FAIL — `undefined: NewFileStore`

- [ ] **Step 3: Write minimal implementation**

`internal/memory/memory.go`:

```go
// Package memory persists the user's preferences and durable facts across
// projects and sessions, so dewey starts a session already knowing how this
// person works rather than rediscovering it every time.
package memory

import "time"

// Memory is one remembered fact or preference.
type Memory struct {
	// ID is a stable slug, also the filename stem. Used for dedup and so a
	// memory can be referred to by hand.
	ID string

	// Text is the memory itself, in plain prose.
	Text string

	// Org gates the memory: empty means it applies anywhere, non-empty means
	// it applies only when the detected org matches exactly. This is
	// deliberately stricter than Tags — see Context.
	Org string

	// Tags are topical (stack, kind of work). Empty means no topical
	// restriction; otherwise at least one must match the context.
	Tags []string

	Created time.Time
}

// Context is what the current working directory implies about the work being
// done. Org is matched exactly and gates; Tags are matched by intersection.
type Context struct {
	Org  string
	Tags []string
}
```

`internal/memory/store.go`:

```go
package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// SkipNotice records a file that could not be loaded. Surfacing these matters:
// a silently ignored memory is a preference the user believes is in effect.
type SkipNotice struct {
	File   string
	Reason string
}

// LoadResult is everything one Load produced, including what it couldn't read.
type LoadResult struct {
	Memories []Memory
	Skipped  []SkipNotice
}

// Store reads and writes memories. Revision is a cheap change token: when it
// differs from a previously observed value, callers must reload.
type Store interface {
	Load(ctx context.Context) (LoadResult, error)
	Save(ctx context.Context, m Memory) error
	Revision(ctx context.Context) string
}

// FileStore is one directory of markdown memories.
type FileStore struct {
	dir string
	gen atomic.Uint64
}

func NewFileStore(dir string) *FileStore {
	return &FileStore{dir: dir}
}

// Dir reports the directory this store reads and writes.
func (s *FileStore) Dir() string { return s.dir }

type frontmatter struct {
	ID      string   `yaml:"id"`
	Org     string   `yaml:"org"`
	Tags    []string `yaml:"tags"`
	Created string   `yaml:"created"`
}

func (s *FileStore) Load(ctx context.Context) (LoadResult, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			// A user who has never saved a memory is not an error.
			return LoadResult{}, nil
		}
		return LoadResult{}, fmt.Errorf("reading memory dir %s: %w", s.dir, err)
	}

	var res LoadResult
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			res.Skipped = append(res.Skipped, SkipNotice{File: e.Name(), Reason: err.Error()})
			continue
		}
		m, err := parseMemory(e.Name(), string(raw))
		if err != nil {
			res.Skipped = append(res.Skipped, SkipNotice{File: e.Name(), Reason: err.Error()})
			continue
		}
		res.Memories = append(res.Memories, m)
	}
	return res, nil
}

// parseMemory splits a memory file into its YAML frontmatter and prose body.
func parseMemory(filename, raw string) (Memory, error) {
	body := raw
	var fm frontmatter

	if strings.HasPrefix(raw, "---\n") {
		rest := raw[len("---\n"):]
		end := strings.Index(rest, "\n---")
		if end < 0 {
			return Memory{}, fmt.Errorf("frontmatter is never closed")
		}
		if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
			return Memory{}, fmt.Errorf("invalid frontmatter: %w", err)
		}
		body = strings.TrimPrefix(rest[end+len("\n---"):], "\n")
	}

	id := fm.ID
	if id == "" {
		id = strings.TrimSuffix(filename, ".md")
	}

	text := strings.TrimSpace(body)
	if text == "" {
		return Memory{}, fmt.Errorf("memory has no body text")
	}

	var created time.Time
	if fm.Created != "" {
		// A bad date shouldn't cost the user the memory; it only affects the
		// order things are dropped in when the budget is tight.
		created, _ = time.Parse("2006-01-02", fm.Created)
	}

	return Memory{ID: id, Text: text, Org: fm.Org, Tags: fm.Tags, Created: created}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory -run TestFileStore -race -v`
Expected: PASS (5 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/memory/memory.go internal/memory/store.go internal/memory/store_test.go
git commit -m "feat(memory): add memory types and markdown-backed file store"
```

---

### Task 2: Store save and revision

**Files:**
- Modify: `internal/memory/store.go`
- Test: `internal/memory/store_test.go`

- [ ] **Step 1: Write the failing test**

```go
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
// directory's mtime does not move when an existing file is overwritten.
func TestFileStore_RevisionChangesWhenAFileIsEditedInPlace(t *testing.T) {
	dir := t.TempDir()
	writeMemFile(t, dir, "x.md", "---\nid: x\n---\nOriginal.\n")
	s := NewFileStore(dir)
	before := s.Revision(context.Background())

	writeMemFile(t, dir, "x.md", "---\nid: x\n---\nRewritten, with different length.\n")

	if after := s.Revision(context.Background()); after == before {
		t.Error("Revision unchanged after an in-place edit — the edit would be invisible until restart")
	}
}
```

Add `"time"` to the test file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory -run 'TestFileStore_(Save|Revision)' -v`
Expected: FAIL — `s.Save undefined` / `s.Revision undefined`

- [ ] **Step 3: Write minimal implementation**

Append to `internal/memory/store.go`:

```go
// validIDPattern keeps an id usable as a filename inside the memory directory.
var validIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func (s *FileStore) Save(ctx context.Context, m Memory) error {
	// The id becomes a path, and ids can originate from model output, so this
	// is a containment check rather than a formatting preference.
	if !validIDPattern.MatchString(m.ID) {
		return fmt.Errorf("invalid memory id %q: want lowercase words joined by hyphens", m.ID)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("creating memory dir %s: %w", s.dir, err)
	}

	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\n", m.ID)
	if !m.Created.IsZero() {
		fmt.Fprintf(&b, "created: %s\n", m.Created.Format("2006-01-02"))
	}
	if m.Org != "" {
		fmt.Fprintf(&b, "org: %s\n", m.Org)
	}
	if len(m.Tags) > 0 {
		fmt.Fprintf(&b, "tags: [%s]\n", strings.Join(m.Tags, ", "))
	}
	b.WriteString("---\n")
	b.WriteString(strings.TrimSpace(m.Text))
	b.WriteString("\n")

	path := filepath.Join(s.dir, m.ID+".md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	s.gen.Add(1)
	return nil
}

// Revision digests the .md entries — name, size, mtime — alongside an
// in-process write counter.
//
// It deliberately does not use the directory's own mtime: that advances only
// when an entry is added or removed, not when an existing file is overwritten,
// so it would miss a memory edited in place, which is the most likely way a
// user curates one. The counter is folded in so this process's own saves
// register regardless of filesystem timestamp granularity.
func (s *FileStore) Revision(ctx context.Context) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d", s.gen.Load())

	// ReadDir returns entries sorted by name, so the digest is stable.
	if entries, err := os.ReadDir(s.dir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			fmt.Fprintf(h, "|%s:%d:%d", e.Name(), info.Size(), info.ModTime().UnixNano())
		}
	}
	return fmt.Sprintf("%x", h.Sum64())
}
```

Add `"hash/fnv"` and `"regexp"` to the imports in `store.go`.

A test must prove this: write a memory, take a revision, rewrite that same file
with different content (adding no new files), and assert the revision changed.
That test fails against a directory-mtime implementation.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory -race -v`
Expected: PASS (8 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/memory/store.go internal/memory/store_test.go
git commit -m "feat(memory): add store save and revision tracking"
```

---

### Task 3: MergedStore

**Files:**
- Create: `internal/memory/merged.go`
- Test: `internal/memory/merged_test.go`

- [ ] **Step 1: Write the failing test**

```go
package memory

import (
	"context"
	"testing"
)

func TestMergedStore_ProjectOverridesGlobalByID(t *testing.T) {
	globalDir, projectDir := t.TempDir(), t.TempDir()
	writeMemFile(t, globalDir, "style.md", "---\nid: style\n---\nGlobal style.\n")
	writeMemFile(t, globalDir, "only-global.md", "---\nid: only-global\n---\nGlobal only.\n")
	writeMemFile(t, projectDir, "style.md", "---\nid: style\n---\nProject style.\n")

	ms := NewMergedStore(NewFileStore(globalDir), NewFileStore(projectDir))
	res, err := ms.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Memories) != 2 {
		t.Fatalf("loaded %d memories, want 2", len(res.Memories))
	}

	byID := map[string]string{}
	for _, m := range res.Memories {
		byID[m.ID] = m.Text
	}
	if byID["style"] != "Project style." {
		t.Errorf("style = %q, want the project copy to win", byID["style"])
	}
	if byID["only-global"] != "Global only." {
		t.Errorf("only-global = %q, want it preserved", byID["only-global"])
	}
}

func TestMergedStore_NilProjectStoreIsFine(t *testing.T) {
	globalDir := t.TempDir()
	writeMemFile(t, globalDir, "a.md", "---\nid: a\n---\nA.\n")

	res, err := NewMergedStore(NewFileStore(globalDir), nil).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Memories) != 1 {
		t.Errorf("loaded %d memories, want 1", len(res.Memories))
	}
}

func TestMergedStore_SavesToGlobal(t *testing.T) {
	globalDir, projectDir := t.TempDir(), t.TempDir()
	global := NewFileStore(globalDir)
	project := NewFileStore(projectDir)

	if err := NewMergedStore(global, project).Save(context.Background(), Memory{ID: "new", Text: "New."}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if res, _ := global.Load(context.Background()); len(res.Memories) != 1 {
		t.Errorf("global has %d memories, want 1", len(res.Memories))
	}
	if res, _ := project.Load(context.Background()); len(res.Memories) != 0 {
		t.Errorf("project has %d memories, want 0 — agent writes belong in the global store", len(res.Memories))
	}
}

func TestMergedStore_RevisionTracksBothStores(t *testing.T) {
	global, project := NewFileStore(t.TempDir()), NewFileStore(t.TempDir())
	ms := NewMergedStore(global, project)

	before := ms.Revision(context.Background())
	if err := project.Save(context.Background(), Memory{ID: "p", Text: "P."}); err != nil {
		t.Fatal(err)
	}
	if after := ms.Revision(context.Background()); after == before {
		t.Error("Revision unchanged after a project-store write")
	}
}

func TestMergedStore_SkipNoticesFromBothStoresAreKept(t *testing.T) {
	globalDir, projectDir := t.TempDir(), t.TempDir()
	writeMemFile(t, globalDir, "bad1.md", "---\nid: [bad\n---\nx\n")
	writeMemFile(t, projectDir, "bad2.md", "---\nid: [bad\n---\nx\n")

	res, _ := NewMergedStore(NewFileStore(globalDir), NewFileStore(projectDir)).Load(context.Background())
	if len(res.Skipped) != 2 {
		t.Errorf("Skipped = %+v, want notices from both stores", res.Skipped)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory -run TestMergedStore -v`
Expected: FAIL — `undefined: NewMergedStore`

- [ ] **Step 3: Write minimal implementation**

```go
package memory

import "context"

// MergedStore layers a project store over a global one, the same way config
// resolves project settings over global ones. Writes go to the global store:
// project memories are meant to be authored and committed deliberately, not
// accumulated by the agent.
type MergedStore struct {
	global  Store
	project Store // nil when running outside a project
}

func NewMergedStore(global, project Store) *MergedStore {
	return &MergedStore{global: global, project: project}
}

func (s *MergedStore) Load(ctx context.Context) (LoadResult, error) {
	res, err := s.global.Load(ctx)
	if err != nil {
		return LoadResult{}, err
	}
	if s.project == nil {
		return res, nil
	}

	projectRes, err := s.project.Load(ctx)
	if err != nil {
		return LoadResult{}, err
	}

	index := make(map[string]int, len(res.Memories))
	for i, m := range res.Memories {
		index[m.ID] = i
	}
	for _, m := range projectRes.Memories {
		if i, ok := index[m.ID]; ok {
			res.Memories[i] = m // documented override, not a conflict
			continue
		}
		index[m.ID] = len(res.Memories)
		res.Memories = append(res.Memories, m)
	}
	res.Skipped = append(res.Skipped, projectRes.Skipped...)
	return res, nil
}

func (s *MergedStore) Save(ctx context.Context, m Memory) error {
	return s.global.Save(ctx, m)
}

func (s *MergedStore) Revision(ctx context.Context) string {
	rev := s.global.Revision(ctx)
	if s.project != nil {
		rev += "|" + s.project.Revision(ctx)
	}
	return rev
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory -race -v`
Expected: PASS (13 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/memory/merged.go internal/memory/merged_test.go
git commit -m "feat(memory): merge project memories over global ones"
```

---

### Task 4: Context detection

**Files:**
- Create: `internal/memory/detect.go`
- Test: `internal/memory/detect_test.go`

- [ ] **Step 1: Write the failing test**

```go
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
		got := DetectContext(dir, "")
		if !contains(got.Tags, c.want) {
			t.Errorf("%s -> Tags %v, want it to contain %q", c.marker, got.Tags, c.want)
		}
	}
}

func TestDetectContext_ProjectOrgWinsOverGitRemote(t *testing.T) {
	dir := t.TempDir()
	if got := DetectContext(dir, "explicit-org"); got.Org != "explicit-org" {
		t.Errorf("Org = %q, want the configured value to take precedence", got.Org)
	}
}

func TestDetectContext_NoMarkersNoRemoteIsEmpty(t *testing.T) {
	got := DetectContext(t.TempDir(), "")
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory -run 'TestDetectContext|TestParseOrg' -v`
Expected: FAIL — `undefined: DetectContext`

- [ ] **Step 3: Write minimal implementation**

```go
package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// stackMarkers maps a file that identifies a stack to the tag it implies.
var stackMarkers = map[string]string{
	"go.mod":           "go",
	"package.json":     "node",
	"Cargo.toml":       "rust",
	"pyproject.toml":   "python",
	"requirements.txt": "python",
}

// DetectContext infers what the work in dir is, so memories can be scoped to
// it. projectOrg comes from the project's config file and wins outright: git
// remote parsing is too fragile to be the only basis for keeping one
// organization's conventions out of another's work.
func DetectContext(dir, projectOrg string) Context {
	var tags []string
	for marker, tag := range stackMarkers {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil && !contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	sortStrings(tags)

	org := projectOrg
	if org == "" {
		org = parseOrgFromRemote(gitRemote(dir))
	}
	return Context{Org: org, Tags: tags}
}

func gitRemote(dir string) string {
	cmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// parseOrgFromRemote pulls the owner segment out of a git remote URL, covering
// the https, scp-style, and ssh:// forms.
func parseOrgFromRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	remote = strings.TrimSuffix(remote, ".git")

	// scp-style: git@host:owner/repo
	if at := strings.Index(remote, "@"); at >= 0 && !strings.Contains(remote, "://") {
		if colon := strings.Index(remote[at:], ":"); colon >= 0 {
			return firstSegment(remote[at+colon+1:])
		}
		return ""
	}

	// URL forms: scheme://[user@]host/owner/repo
	if i := strings.Index(remote, "://"); i >= 0 {
		rest := remote[i+len("://"):]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			return firstSegment(rest[slash+1:])
		}
	}
	return ""
}

func firstSegment(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	if slash := strings.Index(path, "/"); slash >= 0 {
		return path[:slash]
	}
	// A bare segment with no repo after it isn't an owner.
	return ""
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// sortStrings keeps map-derived tag order stable so output is deterministic.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory -race -v`
Expected: PASS (17 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/memory/detect.go internal/memory/detect_test.go
git commit -m "feat(memory): detect stack and organization from the working directory"
```

---

### Task 5: Retriever with the org gate

**Files:**
- Create: `internal/memory/retriever.go`
- Test: `internal/memory/retriever_test.go`

This task carries the isolation guarantee. The `[go, acme]` case below is the
whole reason `Org` is a separate field rather than another tag.

- [ ] **Step 1: Write the failing test**

```go
package memory

import "testing"

func ids(ms []Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func TestTagRetriever_UntaggedMemoryAppliesEverywhere(t *testing.T) {
	all := []Memory{{ID: "global-pref", Text: "x"}}
	got := TagRetriever{}.Select(all, Context{Org: "acme", Tags: []string{"go"}})
	if len(got) != 1 {
		t.Errorf("Select() = %v, want the untagged memory", ids(got))
	}
}

func TestTagRetriever_OrgGatesRegardlessOfTagMatch(t *testing.T) {
	// The bug this prevents: a memory scoped to acme leaking into other work
	// because its topical tag happened to match.
	all := []Memory{{ID: "acme-go-style", Org: "acme", Tags: []string{"go"}, Text: "x"}}

	if got := (TagRetriever{}).Select(all, Context{Org: "acme", Tags: []string{"go"}}); len(got) != 1 {
		t.Errorf("Select() at acme = %v, want the memory to apply", ids(got))
	}
	if got := (TagRetriever{}).Select(all, Context{Org: "other", Tags: []string{"go"}}); len(got) != 0 {
		t.Errorf("Select() at another org = %v, want nothing — org must gate", ids(got))
	}
	if got := (TagRetriever{}).Select(all, Context{Tags: []string{"go"}}); len(got) != 0 {
		t.Errorf("Select() with unknown org = %v, want nothing — fail closed", ids(got))
	}
}

func TestTagRetriever_TagsMatchByIntersection(t *testing.T) {
	all := []Memory{
		{ID: "go-thing", Tags: []string{"go"}, Text: "x"},
		{ID: "node-thing", Tags: []string{"node"}, Text: "x"},
		{ID: "either", Tags: []string{"go", "node"}, Text: "x"},
	}
	got := TagRetriever{}.Select(all, Context{Tags: []string{"go"}})
	if len(got) != 2 || !contains(ids(got), "go-thing") || !contains(ids(got), "either") {
		t.Errorf("Select() = %v, want go-thing and either", ids(got))
	}
}

func TestTagRetriever_TaggedMemoryExcludedWhenContextHasNoTags(t *testing.T) {
	all := []Memory{{ID: "go-thing", Tags: []string{"go"}, Text: "x"}}
	if got := (TagRetriever{}).Select(all, Context{}); len(got) != 0 {
		t.Errorf("Select() = %v, want nothing in an unidentified directory", ids(got))
	}
}

func TestTagRetriever_PreservesInputOrder(t *testing.T) {
	all := []Memory{{ID: "a", Text: "x"}, {ID: "b", Text: "x"}, {ID: "c", Text: "x"}}
	got := ids(TagRetriever{}.Select(all, Context{}))
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("Select() = %v, want input order preserved", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory -run TestTagRetriever -v`
Expected: FAIL — `undefined: TagRetriever`

- [ ] **Step 3: Write minimal implementation**

```go
package memory

// Retriever chooses which memories apply to the current context.
//
// This interface is the seam for smarter retrieval. Tag matching is enough
// while the corpus is small enough to inject wholesale; an embedding-backed
// implementation can replace TagRetriever without the store, the tool, or the
// prompt assembly changing.
type Retriever interface {
	Select(memories []Memory, mc Context) []Memory
}

// TagRetriever selects by exact org match and tag intersection.
type TagRetriever struct{}

var _ Retriever = TagRetriever{}

func (TagRetriever) Select(memories []Memory, mc Context) []Memory {
	var out []Memory
	for _, m := range memories {
		// Org gates: a memory scoped to an organization applies only to that
		// organization's work, and never when the org is unknown. Tags only
		// filter, so a topical match can never override this.
		if m.Org != "" && m.Org != mc.Org {
			continue
		}
		if len(m.Tags) > 0 && !intersects(m.Tags, mc.Tags) {
			continue
		}
		out = append(out, m)
	}
	return out
}

func intersects(a, b []string) bool {
	for _, x := range a {
		if contains(b, x) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory -race -v`
Expected: PASS (22 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/memory/retriever.go internal/memory/retriever_test.go
git commit -m "feat(memory): select memories by org gate and tag intersection"
```

---

### Task 6: Token estimation helper

**Files:**
- Modify: `internal/compaction/tokens.go`
- Test: `internal/compaction/tokens_test.go`

The provider needs to measure the memory block. Reuse the existing cached
tiktoken encoder rather than standing up a second one.

- [ ] **Step 1: Write the failing test**

```go
func TestEstimateText(t *testing.T) {
	if got := EstimateText(""); got != 0 {
		t.Errorf("EstimateText(\"\") = %d, want 0", got)
	}
	short := EstimateText("hello world")
	long := EstimateText(strings.Repeat("hello world ", 50))
	if short <= 0 {
		t.Errorf("EstimateText(short) = %d, want > 0", short)
	}
	if long <= short {
		t.Errorf("EstimateText(long) = %d, not greater than short %d", long, short)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/compaction -run TestEstimateText -v`
Expected: FAIL — `undefined: EstimateText`

- [ ] **Step 3: Write minimal implementation**

Append to `internal/compaction/tokens.go`:

```go
// EstimateText counts tokens in a bare string, sharing the cached encoder with
// EstimateTokens. Returns 0 if the encoder is unavailable, matching
// EstimateTokens' failure direction.
func EstimateText(s string) int {
	if s == "" {
		return 0
	}
	encoder, err := getEncoder()
	if err != nil {
		return 0
	}
	return len(encoder.EncodeOrdinary(s))
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/compaction -race -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/compaction/tokens.go internal/compaction/tokens_test.go
git commit -m "feat(compaction): expose plain-text token estimation"
```

---

### Task 7: Provider — caching, budget, block assembly

**Files:**
- Create: `internal/memory/provider.go`
- Test: `internal/memory/provider_test.go`

- [ ] **Step 1: Write the failing test**

```go
package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// countingStore reports how many times it was actually read, so the cache can
// be observed rather than assumed.
type countingStore struct {
	memories []Memory
	rev      string
	loads    int
}

func (s *countingStore) Load(ctx context.Context) (LoadResult, error) {
	s.loads++
	return LoadResult{Memories: s.memories}, nil
}
func (s *countingStore) Save(ctx context.Context, m Memory) error { return nil }
func (s *countingStore) Revision(ctx context.Context) string      { return s.rev }

func TestProvider_BlockIncludesSelectedMemories(t *testing.T) {
	store := &countingStore{memories: []Memory{
		{ID: "a", Text: "Prefers table-driven tests."},
		{ID: "b", Org: "other", Text: "Should not appear."},
	}}
	p := NewProvider(store, TagRetriever{}, Context{Org: "acme"}, 4000)

	block, err := p.Block(context.Background())
	if err != nil {
		t.Fatalf("Block() error = %v", err)
	}
	if !strings.Contains(block, "Prefers table-driven tests.") {
		t.Errorf("Block() = %q, want the applicable memory", block)
	}
	if strings.Contains(block, "Should not appear.") {
		t.Errorf("Block() = %q, leaked a memory from another org", block)
	}
}

func TestProvider_EmptySelectionYieldsEmptyBlock(t *testing.T) {
	p := NewProvider(&countingStore{}, TagRetriever{}, Context{}, 4000)
	block, err := p.Block(context.Background())
	if err != nil {
		t.Fatalf("Block() error = %v", err)
	}
	if block != "" {
		t.Errorf("Block() = %q, want empty when nothing applies", block)
	}
}

func TestProvider_CachesUntilRevisionChanges(t *testing.T) {
	store := &countingStore{memories: []Memory{{ID: "a", Text: "A."}}, rev: "r1"}
	p := NewProvider(store, TagRetriever{}, Context{}, 4000)

	for i := 0; i < 3; i++ {
		if _, err := p.Block(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if store.loads != 1 {
		t.Errorf("store loaded %d times, want 1 — the cache isn't holding", store.loads)
	}

	store.rev = "r2"
	if _, err := p.Block(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.loads != 2 {
		t.Errorf("store loaded %d times, want 2 — a revision change must force a reload", store.loads)
	}
}

func TestProvider_BudgetDropsOldestFirstAndReportsIt(t *testing.T) {
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &countingStore{memories: []Memory{
		{ID: "old", Text: strings.Repeat("old fact ", 40), Created: old},
		{ID: "recent", Text: strings.Repeat("recent fact ", 40), Created: recent},
	}}
	// Each fixture memory encodes to ~82 tokens, so 100 is the budget that
	// admits exactly one of them — which is what this test is asserting.
	p := NewProvider(store, TagRetriever{}, Context{}, 100)

	block, err := p.Block(context.Background())
	if err != nil {
		t.Fatalf("Block() error = %v", err)
	}
	if strings.Contains(block, "old fact") {
		t.Errorf("Block() = %q, want the oldest memory dropped first", block)
	}
	if !strings.Contains(block, "recent fact") {
		t.Errorf("Block() = %q, want the newest memory kept", block)
	}
	if len(p.Notices()) == 0 {
		t.Error("dropping memories must be reported, not silent")
	}
}

func TestProvider_SkipNoticesSurface(t *testing.T) {
	store := &skipStore{}
	p := NewProvider(store, TagRetriever{}, Context{}, 4000)
	if _, err := p.Block(context.Background()); err != nil {
		t.Fatal(err)
	}
	notices := p.Notices()
	if len(notices) != 1 || !strings.Contains(notices[0], "bad.md") {
		t.Errorf("Notices() = %v, want the skipped file reported", notices)
	}
}

type skipStore struct{}

func (skipStore) Load(ctx context.Context) (LoadResult, error) {
	return LoadResult{Skipped: []SkipNotice{{File: "bad.md", Reason: "invalid frontmatter"}}}, nil
}
func (skipStore) Save(ctx context.Context, m Memory) error { return nil }
func (skipStore) Revision(ctx context.Context) string      { return "r" }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory -run TestProvider -v`
Expected: FAIL — `undefined: NewProvider`

- [ ] **Step 3: Write minimal implementation**

```go
package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/kjkondratuk/dewey/internal/compaction"
)

// Provider turns the stores into the memory block that goes in the system
// prompt, reloading only when the underlying stores actually change.
type Provider struct {
	store     Store
	retriever Retriever
	mc        Context
	maxTokens int

	mu       sync.Mutex
	haveRev  bool
	lastRev  string
	block    string
	notices  []string
}

func NewProvider(store Store, retriever Retriever, mc Context, maxTokens int) *Provider {
	return &Provider{store: store, retriever: retriever, mc: mc, maxTokens: maxTokens}
}

// Block returns the rendered memory block, or "" when nothing applies.
func (p *Provider) Block(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	rev := p.store.Revision(ctx)
	if p.haveRev && rev == p.lastRev {
		return p.block, nil
	}

	res, err := p.store.Load(ctx)
	if err != nil {
		return "", err
	}

	notices := make([]string, 0, len(res.Skipped))
	for _, s := range res.Skipped {
		notices = append(notices, fmt.Sprintf("skipped memory %s: %s", s.File, s.Reason))
	}

	selected := p.retriever.Select(res.Memories, p.mc)
	selected, dropped := applyBudget(selected, p.maxTokens)
	if dropped > 0 {
		// Deliberately not "older memories": selection is newest-first
		// best-fit, so an oversized memory can be skipped while an older but
		// smaller one is kept. Claiming age here would misdescribe it.
		notices = append(notices, fmt.Sprintf(
			"memory budget of %d tokens exceeded; %d memories omitted", p.maxTokens, dropped))
	}

	p.block = render(selected)
	p.notices = notices
	p.lastRev = rev
	p.haveRev = true
	return p.block, nil
}

// Notices reports anything the user should know about the last load: files
// that couldn't be read, memories dropped for budget. Curated content going
// missing silently would be worse than the omission itself.
func (p *Provider) Notices() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.notices...)
}

// applyBudget keeps the newest memories that fit, since a stale preference is
// likelier to have been superseded than a fresh one.
func applyBudget(memories []Memory, maxTokens int) (kept []Memory, dropped int) {
	if maxTokens <= 0 {
		return memories, 0
	}

	// Keyed by position rather than ID: two files can carry the same explicit
	// id, and keying on it would silently keep or drop both together.
	type ranked struct {
		m   Memory
		pos int
	}
	ranking := make([]ranked, len(memories))
	for i, m := range memories {
		ranking[i] = ranked{m: m, pos: i}
	}
	sort.SliceStable(ranking, func(i, j int) bool {
		return ranking[i].m.Created.After(ranking[j].m.Created)
	})

	used := 0
	keep := make([]bool, len(memories))
	for _, r := range ranking {
		cost := compaction.EstimateText("- " + r.m.Text)
		if used+cost > maxTokens {
			dropped++
			continue
		}
		used += cost
		keep[r.pos] = true
	}

	for i, m := range memories { // restore the caller's order
		if keep[i] {
			kept = append(kept, m)
		}
	}
	return kept, dropped
}

func render(memories []Memory) string {
	if len(memories) == 0 {
		return ""
	}
	var b strings.Builder
	for _, m := range memories {
		b.WriteString("- ")
		b.WriteString(strings.Join(strings.Fields(m.Text), " "))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory -race -v`
Expected: PASS (28 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/memory/provider.go internal/memory/provider_test.go
git commit -m "feat(memory): assemble the memory block with caching and a token budget"
```

---

### Task 8: Base system prompt

**Files:**
- Create: `prompts/system_base.md`
- Modify: `prompts/prompts.go`
- Test: `prompts/prompts_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestSystemBaseIsPresentAndMentionsMemoryRules(t *testing.T) {
	if SystemBase == "" {
		t.Fatal("SystemBase is empty — is prompts/system_base.md missing?")
	}
	// The remember tool is useless if the model is never told when to reach
	// for it, and harmful if it records project trivia.
	for _, want := range []string{"remember", "preference"} {
		if !strings.Contains(strings.ToLower(SystemBase), want) {
			t.Errorf("SystemBase does not mention %q", want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./prompts -run TestSystemBase -v`
Expected: FAIL — `undefined: SystemBase`

- [ ] **Step 3: Write minimal implementation**

`prompts/system_base.md`:

```markdown
You are dewey, a command-line coding assistant. You work by running shell
commands and reporting what you find. Be direct and concise: the user is an
experienced engineer reading your output in a terminal.

Tool calls you make may be shown to the user for approval before they run. If
the user denies one, that is their decision, not a limitation on what you can
do — acknowledge it briefly and propose another approach.

You have a `remember` tool that saves a durable fact about the user to their
personal profile, which persists across projects and sessions. Propose a memory
when the user:

- states a preference about how they want work done,
- corrects the same behavior more than once, or
- establishes a fact about their environment that will still be true tomorrow.

Do not record project-specific facts — architecture, where code lives, why a
decision was made. Those belong in the project itself. Record how this person
wants to work, not what this codebase is.

Saving requires the user's approval, so propose rather than announce.
```

Append to `prompts/prompts.go`:

```go
//go:embed system_base.md
var systemBaseMD string
```

and inside the existing `var (...)` block:

```go
	// SystemBase is the base system prompt: how dewey behaves before any of
	// the user's own remembered preferences are layered on.
	SystemBase = strings.TrimSpace(systemBaseMD)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./prompts -race -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add prompts/system_base.md prompts/prompts.go prompts/prompts_test.go
git commit -m "feat(prompts): add the base system prompt"
```

---

### Task 9: Agent holds the system message as a field

**Files:**
- Modify: `internal/agent/openai.go`
- Test: `internal/agent/openai_test.go`

This is the task most likely to break something. The system message must never
enter `history`, or compaction will summarize it away.

- [ ] **Step 1: Write the failing test**

```go
type fakeSystemProvider struct {
	text  string
	calls int
}

func (f *fakeSystemProvider) System(ctx context.Context) (*schema.Message, error) {
	f.calls++
	if f.text == "" {
		return nil, nil
	}
	return &schema.Message{Role: schema.System, Content: f.text}, nil
}

func TestRun_PrependsSystemMessageWithoutStoringIt(t *testing.T) {
	sp := &fakeSystemProvider{text: "SYSTEM RULES"}
	m := &fakeStreamModel{responses: [][]*schema.Message{finalResponse("ok")}}
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, nil, 0, nil)
	agt.systemProvider = sp

	ch, err := agt.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	drain(t, ch, func(Event) bool { return true })

	if len(m.seen) == 0 {
		t.Fatal("model was never called")
	}
	sent := m.seen[0]
	if sent[0].Role != schema.System || sent[0].Content != "SYSTEM RULES" {
		t.Errorf("first message sent = %+v, want the system message", sent[0])
	}
	for _, msg := range agt.history {
		if msg.Role == schema.System {
			t.Error("system message leaked into history — compaction would summarize it away")
		}
	}
}

func TestRun_SystemProviderIsConsultedEveryStream(t *testing.T) {
	// A memory saved mid-turn has to affect the very next model call.
	sp := &fakeSystemProvider{text: "RULES"}
	tl := newRecordingTool(t, new(int))
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
		finalResponse("done"),
	}}
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, nil, 0, nil)
	agt.systemProvider = sp

	ch, _ := agt.Run(context.Background(), "go")
	drain(t, ch, func(Event) bool { return true })

	if sp.calls < 2 {
		t.Errorf("system provider consulted %d times, want once per Stream call", sp.calls)
	}
}

func TestRun_NilSystemProviderSendsHistoryUnchanged(t *testing.T) {
	m := &fakeStreamModel{responses: [][]*schema.Message{finalResponse("ok")}}
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, nil, 0, nil)

	ch, _ := agt.Run(context.Background(), "hello")
	drain(t, ch, func(Event) bool { return true })

	if m.seen[0][0].Role == schema.System {
		t.Error("a system message appeared with no provider configured")
	}
}

func TestCompact_DoesNotTouchTheSystemMessage(t *testing.T) {
	sp := &fakeSystemProvider{text: "RULES"}
	agt := New(nil, nil, nil, nil, 1000, &fakeSummarizer{result: "summary"})
	agt.systemProvider = sp
	agt.history = []*schema.Message{
		{Role: schema.User, Content: "one"},
		{Role: schema.Assistant, Content: "two"},
	}

	if err := agt.compact(context.Background(), 2); err != nil {
		t.Fatalf("compact() error = %v", err)
	}

	sys, _ := sp.System(context.Background())
	if sys == nil || sys.Content != "RULES" {
		t.Error("system message was affected by compaction")
	}
}
```

`fakeStreamModel` must record what it was sent. Add a `seen [][]*schema.Message`
field and append `messages` at the top of its `Stream` method.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent -run 'TestRun_Prepends|TestRun_SystemProvider|TestRun_NilSystem' -v`
Expected: FAIL — `agt.systemProvider undefined`

- [ ] **Step 3: Write minimal implementation**

In `internal/agent/openai.go`, add above `openaiAgent`:

```go
// SystemPromptProvider supplies the system message for each model call. It is
// consulted per Stream rather than once per session so a memory saved mid-turn
// takes effect immediately.
type SystemPromptProvider interface {
	System(ctx context.Context) (*schema.Message, error)
}
```

Add the field to `openaiAgent`:

```go
	// systemProvider is deliberately not part of history: compact() rewrites
	// history[:truncateLen], so a system message stored there would be
	// summarized away the first time compaction fired.
	systemProvider SystemPromptProvider
```

Add the assembly method:

```go
// messages returns what to send: the system message, if any, followed by the
// conversation. The system message is rebuilt per call.
func (a *openaiAgent) messages(ctx context.Context) ([]*schema.Message, error) {
	if a.systemProvider == nil {
		return a.history, nil
	}
	sys, err := a.systemProvider.System(ctx)
	if err != nil {
		return nil, err
	}
	if sys == nil {
		return a.history, nil
	}
	return append([]*schema.Message{sys}, a.history...), nil
}
```

Replace the `Stream` call inside the loop:

```go
			outbound, err := a.messages(ctx)
			if err != nil {
				ch <- Event{Kind: "error", Payload: "system prompt unavailable: " + err.Error()}
				return
			}
			sr, err := a.cm.Stream(ctx, outbound)
```

Update `estimateTokens` so the system message counts toward the context window:

```go
func (a *openaiAgent) estimateTokens() int {
	total := 0
	if a.systemProvider != nil {
		if sys, err := a.systemProvider.System(context.Background()); err == nil && sys != nil {
			total += compaction.EstimateTokens([]*schema.Message{sys})
		}
	}
	if a.lastKnownAt > 0 && a.lastKnownAt <= len(a.history) {
		return total + a.lastKnownTokens + compaction.EstimateTokens(a.history[a.lastKnownAt:])
	}
	return total + compaction.EstimateTokens(a.history)
}
```

Add a setter used by `main.go`:

```go
// WithSystemPrompt attaches a system prompt provider. Separate from New so the
// existing call sites and their tests stay unchanged.
func (a *openaiAgent) WithSystemPrompt(p SystemPromptProvider) *openaiAgent {
	a.systemProvider = p
	return a
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agent -race -count=2 -v`
Expected: PASS — all pre-existing agent tests plus the four new ones

- [ ] **Step 5: Commit**

```bash
git add internal/agent/openai.go internal/agent/openai_test.go
git commit -m "feat(agent): send a system message kept outside compactable history"
```

---

### Task 10: The remember tool

**Files:**
- Create: `internal/memory/recorder.go`
- Modify: `internal/tools/tool_factory.go`
- Test: `internal/memory/recorder_test.go`, `internal/tools/tool_factory_test.go`

- [ ] **Step 1: Write the failing test**

`internal/memory/recorder_test.go`:

```go
package memory

import (
	"context"
	"testing"
	"time"
)

type capturingStore struct{ saved []Memory }

func (s *capturingStore) Load(ctx context.Context) (LoadResult, error) { return LoadResult{}, nil }
func (s *capturingStore) Save(ctx context.Context, m Memory) error {
	s.saved = append(s.saved, m)
	return nil
}
func (s *capturingStore) Revision(ctx context.Context) string { return "r" }

func TestRecorder_StampsOrgFromContextNotInput(t *testing.T) {
	store := &capturingStore{}
	r := NewRecorder(store, Context{Org: "acme", Tags: []string{"go"}})

	if err := r.Record(context.Background(), "Prefers table-driven tests.", []string{"testing"}); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if len(store.saved) != 1 {
		t.Fatalf("saved %d memories, want 1", len(store.saved))
	}
	got := store.saved[0]
	if got.Org != "acme" {
		t.Errorf("Org = %q, want it taken from the detected context", got.Org)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "testing" {
		t.Errorf("Tags = %v, want the caller's tags", got.Tags)
	}
	if got.Created.IsZero() {
		t.Error("Created was not stamped")
	}
}

func TestRecorder_DerivesASlugID(t *testing.T) {
	store := &capturingStore{}
	if err := NewRecorder(store, Context{}).Record(context.Background(), "Prefers Table-Driven Tests!", nil); err != nil {
		t.Fatal(err)
	}
	if got := store.saved[0].ID; got != "prefers-table-driven-tests" {
		t.Errorf("ID = %q, want a lowercase hyphenated slug", got)
	}
}

func TestRecorder_RejectsEmptyText(t *testing.T) {
	if err := NewRecorder(&capturingStore{}, Context{}).Record(context.Background(), "   ", nil); err == nil {
		t.Fatal("expected an error for empty memory text")
	}
}

func TestRecorder_LongTextStillYieldsABoundedID(t *testing.T) {
	store := &capturingStore{}
	long := "This is a very long preference statement that goes on and on describing many things"
	if err := NewRecorder(store, Context{}).Record(context.Background(), long, nil); err != nil {
		t.Fatal(err)
	}
	if got := store.saved[0].ID; len(got) > 60 {
		t.Errorf("ID = %q (%d chars), want it bounded", got, len(got))
	}
	if !validIDPattern.MatchString(store.saved[0].ID) {
		t.Errorf("ID = %q, want it to satisfy the store's id rules", store.saved[0].ID)
	}
}

var _ = time.Now
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory -run TestRecorder -v`
Expected: FAIL — `undefined: NewRecorder`

- [ ] **Step 3: Write minimal implementation**

`internal/memory/recorder.go`:

```go
package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Recorder saves new memories. It exists so the tool layer never chooses the
// org: the model supplies only the text and topical tags, and the org comes
// from the detected context. A model able to set org freely could also set it
// wrongly, and the failure mode is one organization's conventions leaking into
// another's work.
type Recorder struct {
	store Store
	mc    Context
	now   func() time.Time
}

func NewRecorder(store Store, mc Context) *Recorder {
	return &Recorder{store: store, mc: mc, now: time.Now}
}

func (r *Recorder) Record(ctx context.Context, text string, tags []string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("a memory needs some text")
	}
	return r.store.Save(ctx, Memory{
		ID:      slugify(text),
		Text:    text,
		Org:     r.mc.Org,
		Tags:    tags,
		Created: r.now(),
	})
}

// CORRECTED: the original version here split on whitespace and then deleted
// non-alphanumerics *inside* each field, so "Prefers Table-Driven Tests!"
// produced "prefers-tabledriven-tests" — failing this plan's own asserted
// value. Every non-[a-z0-9] rune must act as a separator instead. A length cap
// is also required: a single long word slips past a word-count cap, and the id
// has to satisfy validIDPattern or the memory the user approved is never
// saved. Truncate at a hyphen, since a trailing hyphen fails that pattern.
func slugify(text string) string {
	const maxSlugLength = 60

	var words []string
	var cur strings.Builder
	for _, r := range strings.ToLower(text) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			cur.WriteRune(r)
			continue
		}
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
		if len(words) == 6 {
			break
		}
	}
	if cur.Len() > 0 && len(words) < 6 {
		words = append(words, cur.String())
	}
	if len(words) == 0 {
		return "memory"
	}

	slug := strings.Join(words, "-")
	if len(slug) > maxSlugLength {
		slug = slug[:maxSlugLength]
		if i := strings.LastIndex(slug, "-"); i > 0 {
			slug = slug[:i]
		}
		slug = strings.Trim(slug, "-")
	}
	if slug == "" {
		return "memory"
	}
	return slug
}
```

`internal/tools/tool_factory.go` — add the recorder dependency and the tool:

```go
// MemoryRecorder saves a memory on the user's behalf. Narrow on purpose: the
// tool layer has no business reading memories or choosing their scope.
type MemoryRecorder interface {
	Record(ctx context.Context, text string, tags []string) error
}

type ToolFactory struct {
	recorder MemoryRecorder
}

// NewToolFactory builds the toolset. A nil recorder omits the remember tool,
// which is how memory stays disabled.
func NewToolFactory(recorder MemoryRecorder) *ToolFactory {
	return &ToolFactory{recorder: recorder}
}

type RememberInput struct {
	Text string   `json:"text" jsonschema:"required,description=The fact or preference to remember, as one self-contained sentence of plain prose"`
	Tags []string `json:"tags" jsonschema:"description=Optional topical tags such as go or testing; omit if the memory applies to all work"`
}
```

In `DefaultInvokableToolset`, after `shellCommand`:

```go
	toolset := []tool.InvokableTool{shellCommand}

	if f.recorder != nil {
		remember, _ := utils.InferTool(
			"remember",
			"Saves a durable fact or preference about the user to their personal profile, which persists across projects and sessions. Use for how the user wants work done, not for facts about the current codebase.",
			func(ctx context.Context, in RememberInput) (string, error) {
				if err := f.recorder.Record(ctx, in.Text, in.Tags); err != nil {
					return "", err
				}
				return "Saved.", nil
			},
		)
		toolset = append(toolset, remember)
	}

	return toolset
```

Add to `internal/tools/tool_factory_test.go`:

```go
type fakeRecorder struct {
	text string
	tags []string
	err  error
}

func (f *fakeRecorder) Record(ctx context.Context, text string, tags []string) error {
	f.text, f.tags = text, tags
	return f.err
}

func TestDefaultInvokableToolset_OmitsRememberWithoutARecorder(t *testing.T) {
	for _, tl := range NewToolFactory(nil).DefaultInvokableToolset() {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name == "remember" {
			t.Error("remember tool registered with no recorder configured")
		}
	}
}

func TestRememberTool_RecordsThroughTheRecorder(t *testing.T) {
	rec := &fakeRecorder{}
	var remember tool.InvokableTool
	for _, tl := range NewToolFactory(rec).DefaultInvokableToolset() {
		if info, _ := tl.Info(context.Background()); info.Name == "remember" {
			remember = tl
		}
	}
	if remember == nil {
		t.Fatal("remember tool not registered")
	}

	if _, err := remember.InvokableRun(context.Background(), `{"text":"Prefers mise.","tags":["go"]}`); err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if rec.text != "Prefers mise." {
		t.Errorf("recorded text = %q", rec.text)
	}
	if len(rec.tags) != 1 || rec.tags[0] != "go" {
		t.Errorf("recorded tags = %v, want [go]", rec.tags)
	}
}
```

Update the existing `NewToolFactory()` call sites in the test file to `NewToolFactory(nil)`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory ./internal/tools -race -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/memory/recorder.go internal/memory/recorder_test.go internal/tools/tool_factory.go internal/tools/tool_factory_test.go
git commit -m "feat(memory): add the remember tool backed by a context-scoped recorder"
```

---

### Task 11: Config

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestLoad_MemoryDefaults(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Memory.Enabled {
		t.Error("Memory.Enabled = false, want true by default")
	}
	if cfg.Memory.MaxTokens != defaultMemoryMaxTokens {
		t.Errorf("Memory.MaxTokens = %d, want %d", cfg.Memory.MaxTokens, defaultMemoryMaxTokens)
	}
	if cfg.Memory.Dir == "" {
		t.Error("Memory.Dir is empty, want a default under the home directory")
	}
}

func TestLoad_MemoryCanBeDisabled(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, project, "memory:\n  enabled: false\n")

	cfg, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Memory.Enabled {
		t.Error("Memory.Enabled = true, want false when explicitly disabled")
	}
}

func TestLoad_ProjectOrgIsRead(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, project, "org: acme\n")

	cfg, err := Load(project)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Org != "acme" {
		t.Errorf("Org = %q, want acme", cfg.Org)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config -run 'TestLoad_Memory|TestLoad_ProjectOrg' -v`
Expected: FAIL — `cfg.Memory undefined`

- [ ] **Step 3: Write minimal implementation**

In `internal/config/config.go`:

```go
const defaultMemoryMaxTokens = 4000

// MemoryConfig controls the long-term memory store.
type MemoryConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Dir       string `yaml:"dir"`
	MaxTokens int    `yaml:"max_tokens"`
}
```

Add to `Config`:

```go
	Memory MemoryConfig
	// Org names the organization this project's work belongs to. Set in the
	// project's config file; takes precedence over git-remote detection.
	Org string
```

Add to `fileConfig`:

```go
	// Memory is a pointer so an absent section is distinguishable from one
	// that explicitly sets enabled: false.
	Memory *MemoryConfig `yaml:"memory"`
	Org    string        `yaml:"org"`
```

In `merge`, before the return:

```go
	memory := MemoryConfig{Enabled: true, MaxTokens: defaultMemoryMaxTokens}
	if home, err := os.UserHomeDir(); err == nil {
		memory.Dir = filepath.Join(home, configDirName, "memory")
	}
	// CORRECTED: `Enabled` must be a *bool on the file-config struct, applied
	// only when present. With a plain bool, a project section that sets only
	// `dir:` unmarshals Enabled as false and silently disables memory — asking
	// where memories live would uninstall the feature. A pointer on the
	// section distinguishes an absent section; this distinguishes an absent
	// key. Use a separate memoryFileConfig for the file shape and keep
	// MemoryConfig as the plain resolved struct.
	for _, fc := range []fileConfig{global, project} {
		if fc.Memory == nil {
			continue
		}
		if fc.Memory.Enabled != nil {
			memory.Enabled = *fc.Memory.Enabled
		}
		if fc.Memory.Dir != "" {
			memory.Dir = fc.Memory.Dir
		}
		if fc.Memory.MaxTokens > 0 {
			memory.MaxTokens = fc.Memory.MaxTokens
		}
	}
```

Add `Memory: memory,` and `Org: project.Org,` to the returned `Config`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config -race -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add memory settings and project organization"
```

---

### Task 12: Wire it together

**Files:**
- Create: `internal/memory/system.go`
- Modify: `main.go`
- Test: `internal/memory/system_test.go`

- [ ] **Step 1: Write the failing test**

```go
package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestSystemPrompt_CombinesBaseAndMemories(t *testing.T) {
	store := &countingStore{memories: []Memory{{ID: "a", Text: "Prefers mise."}}, rev: "r"}
	sp := NewSystemPrompt("BASE RULES", NewProvider(store, TagRetriever{}, Context{}, 4000))

	msg, err := sp.System(context.Background())
	if err != nil {
		t.Fatalf("System() error = %v", err)
	}
	if msg.Role != schema.System {
		t.Errorf("Role = %v, want System", msg.Role)
	}
	if !strings.Contains(msg.Content, "BASE RULES") {
		t.Error("base prompt missing")
	}
	if !strings.Contains(msg.Content, "Prefers mise.") {
		t.Error("memory missing")
	}
}

func TestSystemPrompt_NoMemoriesYieldsBaseOnly(t *testing.T) {
	sp := NewSystemPrompt("BASE RULES", NewProvider(&countingStore{rev: "r"}, TagRetriever{}, Context{}, 4000))

	msg, err := sp.System(context.Background())
	if err != nil {
		t.Fatalf("System() error = %v", err)
	}
	if msg.Content != "BASE RULES" {
		t.Errorf("Content = %q, want the base prompt with no memory section", msg.Content)
	}
}

func TestSystemPrompt_NilProviderYieldsBaseOnly(t *testing.T) {
	msg, err := NewSystemPrompt("BASE RULES", nil).System(context.Background())
	if err != nil {
		t.Fatalf("System() error = %v", err)
	}
	if msg.Content != "BASE RULES" {
		t.Errorf("Content = %q, want the base prompt", msg.Content)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory -run TestSystemPrompt -v`
Expected: FAIL — `undefined: NewSystemPrompt`

- [ ] **Step 3: Write minimal implementation**

`internal/memory/system.go`:

```go
package memory

import (
	"context"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// SystemPrompt composes the base instructions with the user's applicable
// memories. It satisfies agent.SystemPromptProvider.
type SystemPrompt struct {
	base     string
	provider *Provider
}

func NewSystemPrompt(base string, provider *Provider) *SystemPrompt {
	return &SystemPrompt{base: base, provider: provider}
}

func (s *SystemPrompt) System(ctx context.Context) (*schema.Message, error) {
	content := s.base
	if s.provider != nil {
		block, err := s.provider.Block(ctx)
		if err != nil {
			return nil, err
		}
		if block != "" {
			var b strings.Builder
			b.WriteString(content)
			b.WriteString("\n\nWhat you know about this user:\n")
			b.WriteString(block)
			content = b.String()
		}
	}
	return &schema.Message{Role: schema.System, Content: content}, nil
}
```

`main.go` — after `contextWindow` is resolved:

```go
	var (
		recorder *memory.Recorder
		// provider stays in the outer scope so Task 13 can read its notices.
		provider     *memory.Provider
		systemPrompt = memory.NewSystemPrompt(prompts.SystemBase, nil)
	)
	if cfg.Memory.Enabled {
		mc := memory.DetectContext(cwd, cfg.Org)
		store := memory.NewMergedStore(
			memory.NewFileStore(cfg.Memory.Dir),
			memory.NewFileStore(filepath.Join(cwd, ".dewey", "memory")),
		)
		provider = memory.NewProvider(store, memory.TagRetriever{}, mc, cfg.Memory.MaxTokens)
		systemPrompt = memory.NewSystemPrompt(prompts.SystemBase, provider)
		recorder = memory.NewRecorder(store, mc)
	}

	var toolRecorder tools2.MemoryRecorder
	if recorder != nil {
		toolRecorder = recorder
	}
	tools := tools2.NewToolFactory(toolRecorder).DefaultInvokableToolset()
```

and change the agent construction:

```go
	agt := agent.New(tm, tools, classifier, cfg.Overrides, contextWindow, summarizer).
		WithSystemPrompt(systemPrompt)
```

Add `path/filepath`, `github.com/kjkondratuk/dewey/internal/memory`, and
`github.com/kjkondratuk/dewey/prompts` to the imports.

Note the `toolRecorder` dance: assigning a nil `*memory.Recorder` directly to
the interface would produce a non-nil interface holding a nil pointer, and the
factory would register the tool anyway.

- [ ] **Step 4: Run test to verify it passes**

Run: `go build ./... && go vet ./... && go test ./... -race -count=2`
Expected: PASS across every package

- [ ] **Step 5: Commit**

```bash
git add internal/memory/system.go internal/memory/system_test.go main.go
git commit -m "feat: wire long-term memory into the agent"
```

---

### Task 13: Surface memory notices to the user

**Files:**
- Modify: `internal/app/app.go`
- Modify: `main.go`
- Test: `internal/app/app_test.go`

The spec requires skipped files and budget drops to be *surfaced* — a memory
the user curated going missing silently is worse than the omission. `Provider`
collects notices but nothing displays them yet.

- [ ] **Step 1: Write the failing test**

```go
func TestNoticesAreShownOnStartup(t *testing.T) {
	f := &fakeAgent{ch: make(chan agent.Event, 8)}
	entry := textinput.New()
	entry.SetVirtualCursor(false)
	m := &App{Conversation: viewport.New(), Entry: entry, Agent: f,
		Notices: []string{"skipped memory bad.md: invalid frontmatter"}}
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	if len(m.turns) != 1 {
		t.Fatalf("turns = %d, want the notice rendered as one turn", len(m.turns))
	}
	if !strings.Contains(m.turns[0].text, "bad.md") {
		t.Errorf("turn text = %q, want the notice", m.turns[0].text)
	}
}

func TestNoNoticesRendersNothing(t *testing.T) {
	f := &fakeAgent{ch: make(chan agent.Event, 8)}
	m := newTestApp(t, f)
	if len(m.turns) != 0 {
		t.Errorf("turns = %d, want none without notices", len(m.turns))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app -run TestNotice -v`
Expected: FAIL — `unknown field Notices`

- [ ] **Step 3: Write minimal implementation**

Add to the `App` struct in `internal/app/app.go`:

```go
	// Notices are startup warnings (unreadable memory files, dropped
	// memories) shown once before the first prompt.
	Notices []string
```

In `Init()`, after the spinner is constructed:

```go
	if len(m.Notices) > 0 {
		m.newLine("system", strings.Join(m.Notices, "\n"))
	}
```

In `main.go`, collect them after building the provider by forcing one load:

```go
	var notices []string
	if cfg.Memory.Enabled {
		if _, err := provider.Block(ctx); err != nil {
			notices = append(notices, "memory unavailable: "+err.Error())
		} else {
			notices = provider.Notices()
		}
	}
```

and pass them through `initModel`:

```go
func initModel(agt agent.Agent, notices []string) *app.App {
	conversation := viewport.New()

	entry := textinput.New()
	entry.Prompt = "> "
	entry.SetVirtualCursor(false)

	return &app.App{Conversation: conversation, Entry: entry, Agent: agt, Notices: notices}
}
```

Note this forced `Block` call also makes memory failures visible at startup
rather than on the first model call. `provider` must be declared in the outer
scope in Task 12 for this to reference it.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/app -race -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/app/app.go internal/app/app_test.go main.go
git commit -m "feat(app): surface memory load notices at startup"
```

---

### Task 14: Verification

- [ ] **Step 1: Full check**

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./... -race -count=3
```

Expected: no output from `gofmt`, everything passes.

- [ ] **Step 2: Manual smoke test**

```bash
mkdir -p ~/.dewey/memory
cat > ~/.dewey/memory/uses-mise.md <<'EOF'
---
id: uses-mise
created: 2026-09-27
tags: [go]
---
Pins Go with mise; the system toolchain is the wrong version.
EOF
go run .
```

Verify: ask "what do you know about how I work?" and confirm the memory is
reflected. Then say "remember that I prefer table-driven tests" and confirm a
confirmation prompt appears showing the proposed memory, that answering `y`
creates a file in `~/.dewey/memory/`, and that answering `n` creates nothing.

- [ ] **Step 3: Verify org isolation by hand**

Create a memory with `org: acme` and confirm via a second directory with a
different git remote that it does not appear.

- [ ] **Step 4: Review**

Dispatch a spec-compliance review and a code-quality review over
`git diff <baseline>..HEAD`, per the pattern used for the previous two features.
