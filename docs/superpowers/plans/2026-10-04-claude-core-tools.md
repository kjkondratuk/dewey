# Claude-Compatible Core Tools Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give dewey Claude Code's core tool vocabulary — `Bash`, `Read`, `Write`, `Edit`, `Grep`, `Glob` — with a deterministic safety floor covering each, so imported skills and agents can later run as written.

**Architecture:** Each tool gets its own file under `internal/tools/`, constructed by the existing `ToolFactory`. `Write` and `Edit` enforce read-before-modify through a new session-scoped `internal/files.Tracker`. The existing `safety.ShellRules` floor already understands non-shell tools via `AlwaysWriteTools`; this adds the mirror concept, `AlwaysReadTools`, for tools whose implementation cannot write — which both prevents spurious approval prompts and skips a classifier round-trip.

**Tech Stack:** Go 1.26, eino (`utils.InferTool` for schema inference), `github.com/yargevad/filepathx` (already an indirect dependency; provides `**` glob support), ripgrep when present on `PATH`.

**Scope note:** This is plan 1 of roughly four against `docs/superpowers/specs/2026-10-04-claude-config-interop-design.md`. It deliberately covers spec phase 1 plus the minimum of phase 2, and **not** phase 0. Rationale in "Deviations from the spec" below.

---

## Deviations from the spec's phasing

The spec groups phases 0–2 as the first plan. Three changes, each flagged so they can be overridden:

**`internal/claudecompat` (phase 0) moves to plan 2.** An inventory of `~/.claude` that nothing consumes is not working software on its own. It belongs at the head of the MCP plan, where `.mcp.json` parsing is its first real consumer.

**`ToolRegistry` moves to plan 2.** Its entire purpose is deferred MCP schema loading. Building it now, with no MCP and nothing to defer, means designing an interface blind. The agent keeps taking `[]tool.InvokableTool` until MCP gives the registry a reason to exist.

**Phase 2's full `rules.tools` glob map and `permissions` import are not needed yet.** `safety.ShellRules.AlwaysWriteTools` already provides a tool-name floor (it is how `remember` is pinned to write). This plan adds only the missing half — `always_read_tools` — and two entries to the existing write list. The glob matching and Claude `permissions` mapping land when MCP introduces hundreds of tool names that need patterns.

Already done ahead of its phase: the context-window clamp and re-discovery, committed on this branch.

---

## File Structure

**Create:**

| File | Responsibility |
| --- | --- |
| `internal/files/tracker.go` | Session record of which files were read, at what mtime; the read-before-modify check |
| `internal/files/tracker_test.go` | Tests for the above |
| `internal/tools/bash.go` | The `Bash` tool (moved out of `tool_factory.go`) |
| `internal/tools/read.go` | The `Read` tool |
| `internal/tools/modify.go` | `Write` and `Edit` — they share the tracker and the same invariants |
| `internal/tools/search.go` | `Grep` and `Glob` |
| `internal/tools/read_test.go`, `modify_test.go`, `search_test.go` | Per-file tests |

**Modify:**

| File | Change |
| --- | --- |
| `internal/tools/tool_factory.go` | Takes a tracker; assembles the six tools; shell code moves to `bash.go` |
| `internal/safety/rules.go` | Add `AlwaysReadTools` field, merge handling, validation, `AlwaysRead` method |
| `internal/safety/classifier.go` | `shellToolName` becomes `"Bash"`; short-circuit always-read tools |
| `internal/safety/default_rules.yaml` | Add `always_read_tools`; add `Write`/`Edit` to `always_write_tools` |
| `main.go` | Construct the tracker, pass to `NewToolFactory` |
| `go.mod` | Promote `filepathx` from indirect to direct |
| `README.md` | Document the tool set and the read-before-modify rule |

Six tools in one `core.go` would be ~400 lines doing six unrelated things. Splitting by tool keeps each file in one context and matches the existing package, where `tool_factory.go` is 98 lines.

---

### Task 1: Session file tracker

**Files:**
- Create: `internal/files/tracker.go`
- Test: `internal/files/tracker_test.go`

- [ ] **Step 1: Write the failing tests**

```go
package files

import (
	"os"
	"path/filepath"
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
```

Add `"strings"` to the import block.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/files/ -v`
Expected: build failure — `undefined: NewTracker`

- [ ] **Step 3: Write minimal implementation**

```go
// Package files tracks which files a session has read, so tools that modify
// files can refuse to work from contents they never looked at.
package files

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Tracker records the files read during one session and the mtime each had
// when it was read.
//
// This exists because Edit and Write are only safe if the model is working
// from the file's current contents. Without it, Edit degrades to blind string
// replacement: a stale old_string either fails to match or, worse, matches
// something the model did not intend, and the failure looks like a model
// mistake rather than a missing precondition.
type Tracker struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func NewTracker() *Tracker {
	return &Tracker{seen: make(map[string]time.Time)}
}

// key normalizes a path so one file read and written through different
// spellings resolves to a single entry.
func key(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return abs
}

// MarkRead records that path was read when it had the given mtime.
func (t *Tracker) MarkRead(path string, mtime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen[key(path)] = mtime
}

// ReadAt returns the mtime path had when it was read this session.
func (t *Tracker) ReadAt(path string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	mtime, ok := t.seen[key(path)]
	return mtime, ok
}

// CheckWritable reports why path must not be modified yet, or nil when it may
// be. A file that does not exist is always writable — requiring a read before
// creating a new file would block the common case.
func (t *Tracker) CheckWritable(path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	readAt, ok := t.ReadAt(path)
	if !ok {
		return fmt.Errorf("%s exists but has not been read this session; read it first so the change is based on its current contents", path)
	}
	if !info.ModTime().Equal(readAt) {
		return fmt.Errorf("%s changed on disk since it was read; read it again before modifying it", path)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/files/ -v`
Expected: all five PASS

- [ ] **Step 5: Commit**

```bash
git add internal/files/
git commit -m "feat(files): track reads so modifications can require one first"
```

---

### Task 2: An always-read floor for tools that cannot write

**Files:**
- Modify: `internal/safety/rules.go`
- Modify: `internal/safety/classifier.go`
- Modify: `internal/safety/default_rules.yaml`
- Test: `internal/safety/rules_test.go`, `internal/safety/classifier_test.go`

`AlwaysWriteTools` already pins a tool to write regardless of the classifier. The mirror is missing: `Read`, `Grep`, and `Glob` cannot modify anything by construction, but the classifier is a local model and will sometimes rate them `moderate`, producing an approval prompt for a directory listing. This is the one place the floor *lowers* a classification, which is safe only because it is a statement about dewey's own code rather than an inference about arbitrary input — so it refuses to apply to `Bash` or to anything also listed as always-write.

- [ ] **Step 1: Write the failing tests**

Append to `internal/safety/rules_test.go`:

```go
func TestAlwaysRead_HonorsTheConfiguredList(t *testing.T) {
	r := ShellRules{AlwaysReadTools: []string{"Read", "Glob"}}

	if !r.AlwaysRead("Read") {
		t.Error("AlwaysRead(Read) = false, want true")
	}
	if r.AlwaysRead("Edit") {
		t.Error("AlwaysRead(Edit) = true, want false for an unlisted tool")
	}
}

func TestAlwaysRead_RefusesTheShellTool(t *testing.T) {
	// A config that could mark the shell tool read-only could disable the
	// approval gate entirely.
	r := ShellRules{AlwaysReadTools: []string{shellToolName}}

	if r.AlwaysRead(shellToolName) {
		t.Errorf("AlwaysRead(%q) = true, want false — the shell tool can never be always-read", shellToolName)
	}
}

func TestAlwaysRead_WriteListWins(t *testing.T) {
	r := ShellRules{
		AlwaysReadTools:  []string{"Edit"},
		AlwaysWriteTools: []string{"Edit"},
	}

	if r.AlwaysRead("Edit") {
		t.Error("AlwaysRead(Edit) = true, want false when it is also always-write")
	}
}

func TestMerge_NoticesAnAlwaysReadEntryThatCannotApply(t *testing.T) {
	base := ShellRules{AlwaysWriteTools: []string{"remember"}}
	user := ShellRules{AlwaysReadTools: []string{"remember", shellToolName, "Read"}}

	merged, notices := base.Merge(user)

	if !merged.AlwaysRead("Read") {
		t.Error("AlwaysRead(Read) = false, want the valid entry kept")
	}
	joined := strings.Join(notices, "\n")
	if !strings.Contains(joined, "remember") || !strings.Contains(joined, shellToolName) {
		t.Errorf("notices = %q, want both rejected entries reported", notices)
	}
}
```

Append to `internal/safety/classifier_test.go`:

```go
func TestClassify_AlwaysReadToolSkipsTheModel(t *testing.T) {
	// Besides avoiding a spurious prompt, this saves a model round-trip on
	// every file read.
	rules := ShellRules{AlwaysReadTools: []string{"Read"}}
	m := &countingModel{}
	c := NewLLMClassifier(m, rules)

	got, err := c.Classify(context.Background(), "Read", "Reads a file", `{"file_path":"/tmp/x"}`)
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	if got.Kind != "read" || got.Risk != Safe {
		t.Errorf("Classify() = %+v, want read/safe", got)
	}
	if got.RequiresApproval() {
		t.Error("RequiresApproval() = true, want false for a tool that cannot write")
	}
	if m.calls != 0 {
		t.Errorf("model calls = %d, want 0", m.calls)
	}
}
```

Add this double to `internal/safety/classifier_test.go` if no equivalent exists:

```go
type countingModel struct {
	calls int
}

func (c *countingModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	c.calls++
	return &schema.Message{Role: schema.Assistant, Content: `{"kind":"write","risk":"dangerous","summary":"x"}`}, nil
}

func (c *countingModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	panic("not used")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/safety/ -run 'TestAlwaysRead|TestMerge_Notices|TestClassify_AlwaysRead' -v`
Expected: build failure — `unknown field AlwaysReadTools`, `r.AlwaysRead undefined`

- [ ] **Step 3: Write minimal implementation**

In `internal/safety/rules.go`, add the field to `ShellRules` after `AlwaysWriteTools`:

```go
	// AlwaysReadTools are tools whose implementation cannot modify anything,
	// so the classifier is skipped for them entirely.
	//
	// This is the only rule that lowers a classification rather than raising
	// it, which is defensible only because it describes dewey's own code: Read
	// opens a file, Glob matches names, Grep searches text, and none of them
	// has a write path. It is not a judgement about arguments, so there is
	// nothing for an argument to smuggle past. AlwaysRead refuses to apply it
	// to the shell tool or to anything also listed as always-write, so a
	// mistaken config cannot turn it into a hole in the approval gate.
	AlwaysReadTools []string `yaml:"always_read_tools"`
```

In the `Merge` method's returned struct literal, alongside the `AlwaysWriteTools` line:

```go
		AlwaysReadTools:  append(append([]string(nil), r.AlwaysReadTools...), user.AlwaysReadTools...),
```

Then, immediately before `Merge` returns at `internal/safety/rules.go:165`, drop entries that can never apply and report them. `notices` is already declared at line 104 and appended to in three places, so this follows the established pattern:

```go
	// An always-read entry that names the shell tool or a tool already pinned
	// to write is ignored rather than honored. Silently dropping it would
	// leave the user believing a rule is in effect, which is the failure this
	// package exists to avoid.
	writeTools := set(merged.AlwaysWriteTools...)
	var keptReads []string
	for _, name := range merged.AlwaysReadTools {
		switch {
		case name == shellToolName:
			notices = append(notices, fmt.Sprintf(
				"ignoring always_read_tools entry %q: the shell tool is never exempt from classification", name))
		case writeTools[name]:
			notices = append(notices, fmt.Sprintf(
				"ignoring always_read_tools entry %q: it is also listed in always_write_tools", name))
		default:
			keptReads = append(keptReads, name)
		}
	}
	merged.AlwaysReadTools = keptReads
```

Add the lookup method next to `ToolFloor` in `internal/safety/shell.go`:

```go
// AlwaysRead reports whether toolName is declared structurally incapable of
// writing, in which case the classifier is not consulted. The two guards are
// deliberately redundant with the validation in Merge: this is the function
// the gate actually calls, so it enforces the invariant itself rather than
// trusting an earlier pass.
func (r ShellRules) AlwaysRead(toolName string) bool {
	if toolName == shellToolName {
		return false
	}
	for _, name := range r.AlwaysWriteTools {
		if name == toolName {
			return false
		}
	}
	for _, name := range r.AlwaysReadTools {
		if name == toolName {
			return true
		}
	}
	return false
}
```

In `internal/safety/classifier.go`, at the top of `Classify`, before `messages` is built:

```go
	// Tools that cannot write are not worth a model round-trip, and a local
	// model rating Read as "moderate" would prompt for something structurally
	// incapable of harm.
	if c.rules.AlwaysRead(toolName) {
		return Classification{Kind: "read", Risk: Safe}, nil
	}
```

In `internal/safety/default_rules.yaml`, extend the write list and add the read list:

```yaml
always_write_tools:
  - remember
  - Write
  - Edit

# Tools whose implementation has no write path at all. Unlike every other rule
# here this one lowers a classification, so it is limited to tools dewey ships
# and is refused for the shell tool. See ShellRules.AlwaysRead.
always_read_tools:
  - Read
  - Grep
  - Glob
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/safety/ -count=1`
Expected: ok — including the pre-existing floor and merge tests

- [ ] **Step 5: Commit**

```bash
git add internal/safety/
git commit -m "feat(safety): add an always-read floor for tools that cannot write"
```

---

### Task 3: Rename the shell tool to Bash

**Files:**
- Create: `internal/tools/bash.go`
- Modify: `internal/tools/tool_factory.go`
- Modify: `internal/safety/classifier.go:28`
- Test: `internal/tools/tool_factory_test.go`, `internal/safety/shell_test.go`, `internal/agent/*_test.go`

Skills and agents reference `Bash`. The implementation is unchanged; only the name and its one reference in the floor move.

- [ ] **Step 1: Write the failing test**

Append to `internal/tools/tool_factory_test.go`:

```go
func TestDefaultInvokableToolset_ShellToolIsNamedBash(t *testing.T) {
	f := NewToolFactory(nil, files.NewTracker())

	var names []string
	for _, tl := range f.DefaultInvokableToolset() {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		names = append(names, info.Name)
	}

	if !slices.Contains(names, "Bash") {
		t.Errorf("tool names = %v, want Bash", names)
	}
	if slices.Contains(names, "run_shell_command") {
		t.Errorf("tool names = %v, want the old run_shell_command name gone", names)
	}
}
```

Add imports: `"slices"` and `"github.com/kjkondratuk/dewey/internal/files"`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tools/ -run TestDefaultInvokableToolset_ShellToolIsNamedBash -v`
Expected: build failure — `too many arguments in call to NewToolFactory`

- [ ] **Step 3: Write minimal implementation**

Create `internal/tools/bash.go`, moving the shell tool out of `tool_factory.go` unchanged except for its name:

```go
package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

type ShellCommandInput struct {
	Command string `json:"command" jsonschema:"required,description=The shell command to execute via sh -c"`
}

// newBashTool builds the shell tool. The name matches Claude Code's, which is
// what imported skills and agent tool allowlists refer to; safety.shellToolName
// has to agree with it or the deterministic shell floor stops applying.
func newBashTool() tool.InvokableTool {
	t, _ := utils.InferTool(
		"Bash",
		"Executes a shell command via sh -c and returns its combined stdout+stderr output plus exit code. Use this for running commands, builds, and tests. Prefer Read, Write, Edit, Grep, and Glob for file operations.",
		func(ctx context.Context, in ShellCommandInput) (string, error) {
			cmd := exec.CommandContext(ctx, "sh", "-c", in.Command)
			output, err := cmd.CombinedOutput()

			var exitErr *exec.ExitError
			if err != nil && !errors.As(err, &exitErr) {
				return "", err
			}

			exitCode := 0
			if exitErr != nil {
				exitCode = exitErr.ExitCode()
			}

			return fmt.Sprintf("%s\nexit code: %d", output, exitCode), nil
		},
	)
	return t
}
```

In `internal/tools/tool_factory.go`: delete the `ShellCommandInput` type and the inline `shellCommand` construction, add the tracker field, and build from the constructor:

```go
type ToolFactory struct {
	recorder MemoryRecorder
	tracker  *files.Tracker
}

// NewToolFactory builds the toolset. A nil recorder omits the remember tool,
// which is how memory stays disabled. tracker is required: Write and Edit
// refuse to modify a file that was not read through it first.
func NewToolFactory(recorder MemoryRecorder, tracker *files.Tracker) *ToolFactory {
	return &ToolFactory{recorder: recorder, tracker: tracker}
}

func (f *ToolFactory) DefaultInvokableToolset() []tool.InvokableTool {
	toolset := []tool.InvokableTool{newBashTool()}

	if f.recorder != nil {
		// ... existing remember construction, unchanged ...
		toolset = append(toolset, remember)
	}

	return toolset
}
```

In `internal/safety/classifier.go:28`:

```go
const shellToolName = "Bash"
```

Update every remaining reference. There are 27 across 9 files; confirm with:

```bash
grep -rn "run_shell_command" . --exclude-dir=.git --exclude-dir=docs
```

Expected locations, all mechanical renames:

| File | Refs | Note |
| --- | --- | --- |
| `internal/safety/classifier.go:28` | 1 | the `shellToolName` constant itself |
| `internal/tools/tool_factory.go:57` | 1 | removed by moving the tool to `bash.go` |
| `internal/tools/tool_factory_test.go:23-24` | 2 | superseded by the new test above |
| `internal/agent/openai_test.go` | 4 | `formatConfirmMessage` assertions, including expected prompt text |
| `internal/agent/approval_gate_test.go` | 2 | `runAndAnswer` tool-name arguments |
| `internal/agent/openai.go:570` | 1 | comment only |
| `internal/compaction/summarizer_test.go` | 4 | transcript fixtures and an expected-order assertion |
| `internal/compaction/tokens_test.go` | 1 | fixture |
| `README.md:233,313,347,600` | 4 | prose and one sample confirmation prompt |

The `prompts/*.md` files contain no reference, so the system and classifier
prompts need no change.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -count=1`
Expected: all packages ok

- [ ] **Step 5: Commit**

```bash
git add internal/tools/ internal/safety/ internal/agent/ main.go prompts/ README.md
git commit -m "refactor(tools): rename run_shell_command to Bash"
```

---

### Task 4: The Read tool

**Files:**
- Create: `internal/tools/read.go`
- Test: `internal/tools/read_test.go`

- [ ] **Step 1: Write the failing tests**

```go
package tools

import (
	"context"
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tools/ -run TestRead -v`
Expected: build failure — `undefined: readFile`, `undefined: ReadInput`

- [ ] **Step 3: Write minimal implementation**

```go
package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/kjkondratuk/dewey/internal/files"
)

// defaultReadLimit caps an unbounded Read. Matches Claude Code's default, so
// skills that assume "Read gives me the first 2000 lines" hold.
const defaultReadLimit = 2000

type ReadInput struct {
	FilePath string `json:"file_path" jsonschema:"required,description=Absolute path to the file to read"`
	Offset   int    `json:"offset" jsonschema:"description=Optional. 1-indexed line number to start from. Omit to start at the beginning"`
	Limit    int    `json:"limit" jsonschema:"description=Optional. Maximum number of lines to return. Omit for 2000"`
}

// readFile is the tool body, separated from registration so it can be tested
// without going through JSON schema inference.
func readFile(tracker *files.Tracker, in ReadInput) (string, error) {
	if !filepath.IsAbs(in.FilePath) {
		return "", fmt.Errorf("file_path must be absolute, got %q", in.FilePath)
	}

	f, err := os.Open(in.FilePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory; use Glob or Bash to list it", in.FilePath)
	}

	offset := in.Offset
	if offset < 1 {
		offset = 1
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}

	var b strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	lineNo, emitted := 0, 0
	for scanner.Scan() {
		lineNo++
		if lineNo < offset {
			continue
		}
		if emitted >= limit {
			fmt.Fprintf(&b, "\n[truncated at %d lines; re-read with offset=%d for more]\n", limit, lineNo)
			break
		}
		fmt.Fprintf(&b, "%6d\t%s\n", lineNo, scanner.Text())
		emitted++
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// Record the read before returning: this is what lets Write and Edit touch
	// the file, and an unrecorded successful read would block them for no
	// reason the model could diagnose.
	tracker.MarkRead(in.FilePath, info.ModTime())

	if emitted == 0 {
		if lineNo == 0 {
			return fmt.Sprintf("%s is empty.", in.FilePath), nil
		}
		return fmt.Sprintf("%s has %d lines; offset %d is past the end.", in.FilePath, lineNo, offset), nil
	}
	return b.String(), nil
}

func newReadTool(tracker *files.Tracker) tool.InvokableTool {
	t, _ := utils.InferTool(
		"Read",
		"Reads a file from the filesystem and returns its contents with line numbers. Reads up to 2000 lines by default; use offset and limit for a specific range. Must be called before Write or Edit can modify an existing file.",
		func(ctx context.Context, in ReadInput) (string, error) {
			return readFile(tracker, in)
		},
	)
	return t
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tools/ -run TestRead -v`
Expected: all five PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tools/read.go internal/tools/read_test.go
git commit -m "feat(tools): add the Read tool"
```

---

### Task 5: The Write and Edit tools

**Files:**
- Create: `internal/tools/modify.go`
- Test: `internal/tools/modify_test.go`

- [ ] **Step 1: Write the failing tests**

```go
package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tools/ -run 'TestWrite|TestEdit' -v`
Expected: build failure — `undefined: writeFile`, `undefined: editFile`

- [ ] **Step 3: Write minimal implementation**

```go
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/kjkondratuk/dewey/internal/files"
)

type WriteInput struct {
	FilePath string `json:"file_path" jsonschema:"required,description=Absolute path to the file to write"`
	Content  string `json:"content" jsonschema:"required,description=The complete contents to write. Replaces the file entirely"`
}

type EditInput struct {
	FilePath   string `json:"file_path" jsonschema:"required,description=Absolute path to the file to edit"`
	OldString  string `json:"old_string" jsonschema:"required,description=The exact text to replace. Must appear exactly once unless replace_all is true"`
	NewString  string `json:"new_string" jsonschema:"required,description=The text to replace it with. Must differ from old_string"`
	ReplaceAll bool   `json:"replace_all" jsonschema:"description=Optional. Replace every occurrence instead of requiring exactly one"`
}

// persist writes content to path and re-records the read, so a following
// modification is not rejected for the mtime this write just changed.
func persist(tracker *files.Tracker, path, content string, perm os.FileMode) error {
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tracker.MarkRead(path, info.ModTime())
	return nil
}

func writeFile(tracker *files.Tracker, in WriteInput) (string, error) {
	if !filepath.IsAbs(in.FilePath) {
		return "", fmt.Errorf("file_path must be absolute, got %q", in.FilePath)
	}
	if err := tracker.CheckWritable(in.FilePath); err != nil {
		return "", err
	}

	perm := os.FileMode(0o644)
	if info, err := os.Stat(in.FilePath); err == nil {
		perm = info.Mode().Perm()
	}

	if err := persist(tracker, in.FilePath, in.Content, perm); err != nil {
		return "", err
	}
	return fmt.Sprintf("Wrote %d bytes to %s.", len(in.Content), in.FilePath), nil
}

func editFile(tracker *files.Tracker, in EditInput) (string, error) {
	if !filepath.IsAbs(in.FilePath) {
		return "", fmt.Errorf("file_path must be absolute, got %q", in.FilePath)
	}
	if in.OldString == in.NewString {
		return "", fmt.Errorf("old_string and new_string are identical; nothing to change")
	}
	if err := tracker.CheckWritable(in.FilePath); err != nil {
		return "", err
	}

	info, err := os.Stat(in.FilePath)
	if err != nil {
		return "", err
	}
	original, err := os.ReadFile(in.FilePath)
	if err != nil {
		return "", err
	}
	text := string(original)

	count := strings.Count(text, in.OldString)
	switch {
	case count == 0:
		return "", fmt.Errorf("old_string not found in %s; read the file again to see its current contents", in.FilePath)
	case count > 1 && !in.ReplaceAll:
		// Refusing is the whole point: replacing the first of several matches
		// edits a line the model did not choose, and nothing downstream can
		// detect that it happened.
		return "", fmt.Errorf("old_string appears %d times in %s; make it unique or set replace_all", count, in.FilePath)
	}

	updated := text
	replaced := count
	if in.ReplaceAll {
		updated = strings.ReplaceAll(text, in.OldString, in.NewString)
	} else {
		updated = strings.Replace(text, in.OldString, in.NewString, 1)
		replaced = 1
	}

	if err := persist(tracker, in.FilePath, updated, info.Mode().Perm()); err != nil {
		return "", err
	}
	return fmt.Sprintf("Replaced %d occurrence(s) in %s.", replaced, in.FilePath), nil
}

func newWriteTool(tracker *files.Tracker) tool.InvokableTool {
	t, _ := utils.InferTool(
		"Write",
		"Writes a file to the filesystem, replacing it entirely if it exists. An existing file must be read first. For changing part of a file, prefer Edit.",
		func(ctx context.Context, in WriteInput) (string, error) {
			return writeFile(tracker, in)
		},
	)
	return t
}

func newEditTool(tracker *files.Tracker) tool.InvokableTool {
	t, _ := utils.InferTool(
		"Edit",
		"Performs an exact string replacement in a file. The file must be read first. old_string must appear exactly once unless replace_all is set, so an ambiguous edit fails instead of changing the wrong line.",
		func(ctx context.Context, in EditInput) (string, error) {
			return editFile(tracker, in)
		},
	)
	return t
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tools/ -run 'TestWrite|TestEdit' -v`
Expected: all ten PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tools/modify.go internal/tools/modify_test.go
git commit -m "feat(tools): add Write and Edit with read-before-modify"
```

---

### Task 6: The Glob tool

**Files:**
- Create: `internal/tools/search.go`
- Test: `internal/tools/search_test.go`
- Modify: `go.mod`

`filepathx` is already an indirect dependency and supports `**`, which `path/filepath.Glob` does not.

- [ ] **Step 1: Write the failing tests**

```go
package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tools/ -run TestGlob -v`
Expected: build failure — `undefined: globFiles`, `undefined: GlobInput`

- [ ] **Step 3: Write minimal implementation**

```go
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/yargevad/filepathx"
)

// maxSearchResults caps Glob and Grep output. An unbounded file list in a
// monorepo can exceed the whole context window on its own.
const maxSearchResults = 200

type GlobInput struct {
	Pattern string `json:"pattern" jsonschema:"required,description=Glob pattern such as **/*.go or src/**/*.ts"`
	Path     string `json:"path" jsonschema:"description=Optional. Absolute directory to search in. Defaults to the working directory"`
}

func searchRoot(path string) (string, error) {
	if path == "" {
		return os.Getwd()
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute, got %q", path)
	}
	return path, nil
}

func globFiles(in GlobInput) (string, error) {
	root, err := searchRoot(in.Path)
	if err != nil {
		return "", err
	}

	matches, err := filepathx.Glob(filepath.Join(root, in.Pattern))
	if err != nil {
		return "", err
	}

	// Most-recently-modified first: when a pattern matches more than fits,
	// the files someone is currently working on are the ones worth showing.
	type entry struct {
		path string
		mod  int64
	}
	entries := make([]entry, 0, len(matches))
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil || info.IsDir() {
			continue
		}
		entries = append(entries, entry{path: m, mod: info.ModTime().UnixNano()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod > entries[j].mod })

	if len(entries) == 0 {
		return fmt.Sprintf("No files matched %q under %s.", in.Pattern, root), nil
	}

	var b strings.Builder
	for i, e := range entries {
		if i >= maxSearchResults {
			fmt.Fprintf(&b, "\n[%d more matches not shown; narrow the pattern]\n", len(entries)-maxSearchResults)
			break
		}
		b.WriteString(e.path)
		b.WriteString("\n")
	}
	return b.String(), nil
}

func newGlobTool() tool.InvokableTool {
	t, _ := utils.InferTool(
		"Glob",
		"Finds files by glob pattern, supporting ** for recursive matching. Returns paths sorted by modification time, newest first. Use this to locate files by name; use Grep to search their contents.",
		func(ctx context.Context, in GlobInput) (string, error) {
			return globFiles(in)
		},
	)
	return t
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tools/ -run TestGlob -v && go mod tidy && go build ./...`
Expected: all four PASS; `filepathx` moves out of the indirect block in `go.mod`

- [ ] **Step 5: Commit**

```bash
git add internal/tools/search.go internal/tools/search_test.go go.mod go.sum
git commit -m "feat(tools): add the Glob tool"
```

---

### Task 7: The Grep tool

**Files:**
- Modify: `internal/tools/search.go`
- Test: `internal/tools/search_test.go`

Uses ripgrep when it is on `PATH`, otherwise a Go walk. A hard `rg` dependency would be a surprising install requirement for a single Go binary, and the fallback has to produce the same output shape or behavior would depend on what is installed.

**Scope:** `output_mode` supports `content`, `files_with_matches`, and `count`; flags are `-i` and `-n`. Context lines (`-A`/`-B`/`-C`) are deliberately omitted — they require rg-equivalent grouping logic in the fallback for little benefit when `Read` with an offset is available. Note this in the tool description so the model does not attempt them.

- [ ] **Step 1: Write the failing tests**

```go
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
	seed(t, dir, "hit.go", "miss.go")
	if err := os.WriteFile(filepath.Join(dir, "hit.go"), []byte("needle\n"), 0o644); err != nil {
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
	_, err := grepFiles(GrepInput{Pattern: "([unclosed", Path: t.TempDir(), OutputMode: "content"})
	if err == nil {
		t.Error("grepFiles(invalid regex) = nil, want an error")
	}
}

func TestGrepGoFallback_MatchesRipgrepOutputShape(t *testing.T) {
	// The fallback runs when rg is absent, so its output has to be usable in
	// the same way rather than merely non-empty.
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tools/ -run TestGrep -v`
Expected: build failure — `undefined: grepFiles`, `undefined: GrepInput`, `undefined: grepWithGo`

- [ ] **Step 3: Write minimal implementation**

Append to `internal/tools/search.go` (and add `"bufio"`, `"os/exec"`, `"regexp"`, `"strconv"` to the imports):

```go
type GrepInput struct {
	Pattern     string `json:"pattern" jsonschema:"required,description=Regular expression to search for"`
	Path        string `json:"path" jsonschema:"description=Optional. Absolute directory to search in. Defaults to the working directory"`
	Glob        string `json:"glob" jsonschema:"description=Optional. Restrict the search to files matching this glob, such as *.go"`
	OutputMode  string `json:"output_mode" jsonschema:"description=One of content, files_with_matches, or count. Defaults to files_with_matches"`
	IgnoreCase  bool   `json:"-i" jsonschema:"description=Optional. Case-insensitive search"`
	LineNumbers bool   `json:"-n" jsonschema:"description=Optional. Include line numbers in content output"`
}

func (in GrepInput) mode() string {
	switch in.OutputMode {
	case "content", "count", "files_with_matches":
		return in.OutputMode
	default:
		return "files_with_matches"
	}
}

// grepFiles searches with ripgrep when it is available and falls back to a Go
// walk otherwise. Both paths produce path:line:text for content mode so the
// model sees one format regardless of what is installed.
func grepFiles(in GrepInput) (string, error) {
	// Compile first either way: rg would report a bad pattern as a generic
	// exit code, and an invalid regex should be a clear error rather than
	// "no matches".
	if _, err := regexp.Compile(in.Pattern); err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}
	if _, err := searchRoot(in.Path); err != nil {
		return "", err
	}

	if path, err := exec.LookPath("rg"); err == nil {
		return grepWithRipgrep(path, in)
	}
	return grepWithGo(in)
}

func grepWithRipgrep(rg string, in GrepInput) (string, error) {
	root, err := searchRoot(in.Path)
	if err != nil {
		return "", err
	}

	args := []string{"--color=never"}
	switch in.mode() {
	case "files_with_matches":
		args = append(args, "--files-with-matches")
	case "count":
		args = append(args, "--count")
	default:
		args = append(args, "--no-heading", "--with-filename")
		if in.LineNumbers {
			args = append(args, "--line-number")
		}
	}
	if in.IgnoreCase {
		args = append(args, "--ignore-case")
	}
	if in.Glob != "" {
		args = append(args, "--glob", in.Glob)
	}
	args = append(args, "--regexp", in.Pattern, root)

	out, err := exec.Command(rg, args...).Output()
	// rg exits 1 for "no matches", which is not a failure.
	var exitErr *exec.ExitError
	if err != nil && (!errors.As(err, &exitErr) || exitErr.ExitCode() != 1) {
		return "", err
	}
	return truncateMatches(string(out), in)
}

func grepWithGo(in GrepInput) (string, error) {
	root, err := searchRoot(in.Path)
	if err != nil {
		return "", err
	}

	expr := in.Pattern
	if in.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}

	var b strings.Builder
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if d.IsDir() {
			// .git holds packed binary objects that match almost any pattern.
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if in.Glob != "" {
			ok, err := filepath.Match(in.Glob, d.Name())
			if err != nil || !ok {
				return nil
			}
		}

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
		lineNo, hits := 0, 0
		for scanner.Scan() {
			lineNo++
			if !re.MatchString(scanner.Text()) {
				continue
			}
			hits++
			if in.mode() == "content" {
				if in.LineNumbers {
					fmt.Fprintf(&b, "%s:%d:%s\n", path, lineNo, scanner.Text())
				} else {
					fmt.Fprintf(&b, "%s:%s\n", path, scanner.Text())
				}
			}
		}
		switch {
		case hits == 0:
		case in.mode() == "files_with_matches":
			fmt.Fprintf(&b, "%s\n", path)
		case in.mode() == "count":
			fmt.Fprintf(&b, "%s:%s\n", path, strconv.Itoa(hits))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return truncateMatches(b.String(), in)
}

// truncateMatches caps output and turns an empty result into a statement, so
// "found nothing" is distinguishable from "the call failed".
func truncateMatches(out string, in GrepInput) (string, error) {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return fmt.Sprintf("No matches for %q.", in.Pattern), nil
	}
	lines := strings.Split(out, "\n")
	if len(lines) > maxSearchResults {
		return strings.Join(lines[:maxSearchResults], "\n") +
			fmt.Sprintf("\n[%d more lines not shown; narrow the pattern or use output_mode=files_with_matches]", len(lines)-maxSearchResults), nil
	}
	return out + "\n", nil
}

func newGrepTool() tool.InvokableTool {
	t, _ := utils.InferTool(
		"Grep",
		"Searches file contents with a regular expression. output_mode selects content (matching lines), files_with_matches (paths only, the default), or count. Supports -i for case-insensitive and -n for line numbers. Context lines are not supported; use Read with an offset instead.",
		func(ctx context.Context, in GrepInput) (string, error) {
			return grepFiles(in)
		},
	)
	return t
}
```

Add `"errors"` to the import block for the `exec.ExitError` check.

**Contingency on the `-i` and `-n` field names.** These match Claude's
parameter names so a model trained on them emits the right JSON, but they are
unusual Go struct tags. Only the exact tag `-` means "skip this field", so
`json:"-i"` is legal — however `utils.InferTool` runs the struct through
schema inference, and a leading hyphen in a property name has not been
verified against that path.

Assert it directly rather than assuming — add this test, confirm it passes,
and keep it (it needs `"encoding/json"` in the import block):

```go
func TestGrep_SchemaCarriesClaudeFlagNames(t *testing.T) {
	info, err := newGrepTool().Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.ParamsOneOf == nil {
		t.Fatal("ParamsOneOf = nil, want an inferred schema")
	}
	// The assertion that matters is that inference did not drop or mangle the
	// hyphenated names; marshalling the schema is the cheapest way to see it.
	raw, err := json.Marshal(info.ParamsOneOf)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, want := range []string{"-i", "-n"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("schema = %s, want it to carry the %q parameter", raw, want)
		}
	}
}
```

If that test cannot be made to pass, rename the two fields to
`case_insensitive` and `line_numbers`, note the divergence in the tool
description so the model is told the correct names, and carry it as a known
fidelity gap rather than fighting the schema library.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tools/ -run TestGrep -v`
Expected: all seven PASS. `TestGrepGoFallback_MatchesRipgrepOutputShape` exercises the Go path directly, so it passes whether or not `rg` is installed.

- [ ] **Step 5: Commit**

```bash
git add internal/tools/search.go internal/tools/search_test.go
git commit -m "feat(tools): add the Grep tool with a ripgrep-free fallback"
```

---

### Task 8: Register the tools and wire the tracker

**Files:**
- Modify: `internal/tools/tool_factory.go`
- Modify: `main.go:70-78`
- Test: `internal/tools/tool_factory_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestDefaultInvokableToolset_ExposesTheClaudeCoreTools(t *testing.T) {
	f := NewToolFactory(nil, files.NewTracker())

	have := map[string]bool{}
	for _, tl := range f.DefaultInvokableToolset() {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		have[info.Name] = true
	}

	for _, want := range []string{"Bash", "Read", "Write", "Edit", "Grep", "Glob"} {
		if !have[want] {
			t.Errorf("tool %q missing from the toolset", want)
		}
	}
	if have["remember"] {
		t.Error("remember present with a nil recorder, want it omitted")
	}
}

func TestDefaultInvokableToolset_EveryToolHasASafetyClassification(t *testing.T) {
	// A tool in neither list falls through to the classifier alone, which is
	// how a write ends up unprompted. This catches a tool added without a
	// matching rule.
	rules, err := safety.DefaultShellRules()
	if err != nil {
		t.Fatalf("DefaultShellRules() error = %v", err)
	}
	f := NewToolFactory(nil, files.NewTracker())

	for _, tl := range f.DefaultInvokableToolset() {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		if info.Name == "Bash" {
			continue // classified by inspecting the command, not by name
		}
		floor := rules.ToolFloor(info.Name, "{}")
		if floor.Kind != "write" && !rules.AlwaysRead(info.Name) {
			t.Errorf("tool %q is in neither always_write_tools nor always_read_tools", info.Name)
		}
	}
}
```

Add `"github.com/kjkondratuk/dewey/internal/safety"` to the imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tools/ -run TestDefaultInvokableToolset -v`
Expected: FAIL — `tool "Read" missing from the toolset` and the same for Write, Edit, Grep, Glob

- [ ] **Step 3: Write minimal implementation**

In `internal/tools/tool_factory.go`, replace the body of `DefaultInvokableToolset`:

```go
func (f *ToolFactory) DefaultInvokableToolset() []tool.InvokableTool {
	toolset := []tool.InvokableTool{
		newBashTool(),
		newReadTool(f.tracker),
		newWriteTool(f.tracker),
		newEditTool(f.tracker),
		newGrepTool(),
		newGlobTool(),
	}

	if f.recorder != nil {
		remember, _ := utils.InferTool(
			"remember",
			"Saves a durable fact or preference about the user to their personal profile, which persists across projects and sessions. Use for how the user wants work done, not for facts about the current codebase.",
			func(ctx context.Context, in RememberInput) (string, error) {
				if err := f.recorder.RecordScoped(ctx, in.Text, in.Tags, in.ThisOrgOnly); err != nil {
					return "", err
				}
				return "Saved.", nil
			},
		)
		toolset = append(toolset, remember)
	}

	return toolset
}
```

In `main.go`, construct the tracker and pass it (replacing the `tools2.NewToolFactory(toolRecorder)` call):

```go
	// One tracker per session: Write and Edit consult it to refuse modifying a
	// file that was not read first.
	tracker := files.NewTracker()
	tools := tools2.NewToolFactory(toolRecorder, tracker).DefaultInvokableToolset()
```

Add `"github.com/kjkondratuk/dewey/internal/files"` to `main.go`'s imports.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go vet ./... && gofmt -l . internal prompts && go test ./... -count=1`
Expected: build ok, vet silent, gofmt silent, all packages ok

- [ ] **Step 5: Commit**

```bash
git add internal/tools/ main.go
git commit -m "feat(tools): register the Claude core toolset"
```

---

### Task 9: Document the tool set

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Add a tools section**

Insert after the "Quick start" section, before "How the model is chosen":

```markdown
## Tools

| Tool | What it does | Approval |
| --- | --- | --- |
| `Bash` | Runs a shell command via `sh -c` | Per the shell rules below |
| `Read` | Reads a file with line numbers, 2000 lines by default | Never prompts |
| `Write` | Replaces a file's contents entirely | Always prompts |
| `Edit` | Exact string replacement in a file | Always prompts |
| `Grep` | Regex search over file contents | Never prompts |
| `Glob` | Finds files by glob pattern, `**` supported | Never prompts |
| `remember` | Saves a preference to your profile | Always prompts |

The names match Claude Code's deliberately, so skills and subagents written
for it can run here unchanged once that work lands.

`Read`, `Grep`, and `Glob` never prompt because they have no write path at
all — this is declared in `always_read_tools` and skips the classifier
entirely, which also saves a model round-trip per call. `Write` and `Edit` are
in `always_write_tools`, so they prompt no matter what the classifier thinks.

### Read before modify

`Write` and `Edit` refuse to touch an **existing** file that has not been read
this session, and refuse again if it changed on disk since that read. Creating
a new file needs no prior read.

This exists because `Edit` requires `old_string` to match exactly once. Without
the precondition, a stale `old_string` either fails to match or matches
something the model did not intend, and neither failure is detectable
afterwards. The error tells the model to read the file again, which is the fix.

`Edit` also refuses an `old_string` that appears more than once unless
`replace_all` is set, rather than editing the first occurrence.
```

- [ ] **Step 2: Verify the docs match the code**

Run: `grep -n "always_read_tools\|always_write_tools" internal/safety/default_rules.yaml`
Expected: both keys present, listing exactly the tools the table claims

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document the Claude core tool set"
```

---

## Out of scope, deliberately

| Omission | Why |
| --- | --- |
| `ToolRegistry`, deferred loading, `ToolSearch` | No MCP yet, so nothing to defer. Plan 2. |
| `internal/claudecompat` | Produces an inventory with no consumer until MCP. Plan 2. |
| `rules.tools` glob patterns, Claude `permissions` import | `AlwaysWriteTools`/`AlwaysReadTools` cover a handful of known names; patterns earn their keep when MCP brings hundreds. |
| Grep context lines (`-A`/`-B`/`-C`) | Needs rg-equivalent grouping in the fallback; `Read` with an offset covers the need. Stated in the tool description so the model does not try. |
| `NotebookEdit`, `WebFetch`, `TodoWrite`, `Task`, `Skill` | Not core-tool work. `Task` and `Skill` are plans 3 and 4. |
| Multi-edit / `MultiEdit` | `Edit` with `replace_all` covers the common case. |

## Verification before calling this done

```bash
go build ./... && go vet ./... && gofmt -l . internal prompts && go test ./... -count=1
```

All four must be clean. Then confirm by hand, since no test covers the wiring end to end:

1. Start dewey, ask it to read a file. Expect no approval prompt.
2. Ask it to edit that file. Expect one approval prompt showing the change.
3. Ask it to edit a file it has not read. Expect the read-first error, then a successful retry after it reads.
