package safety

import (
	"strings"
	"testing"
)

func TestDefaultShellRules_LoadAndCompile(t *testing.T) {
	r, err := DefaultShellRules()
	if err != nil {
		t.Fatalf("DefaultShellRules() error = %v", err)
	}
	if len(r.AlwaysWrite) == 0 || len(r.ReadsWhen) == 0 || len(r.WritesWithFlags) == 0 {
		t.Fatal("built-in rules loaded empty")
	}
	if !contains(r.AlwaysWriteTools, "remember") {
		t.Error("remember is not declared a writing tool")
	}
	// Every program with patterns must have compiled them.
	for program, rules := range r.ReadsWhen {
		if len(rules.Patterns) > 0 && len(r.compiled[program]) != len(rules.Patterns) {
			t.Errorf("%s: %d patterns but %d compiled", program, len(rules.Patterns), len(r.compiled[program]))
		}
	}
}

// Adding a tool must not discard the built-ins for it. This is the whole point
// of config: covering terraform shouldn't mean rewriting the git rules.
func TestMerge_ExtendsRatherThanReplaces(t *testing.T) {
	base, err := DefaultShellRules()
	if err != nil {
		t.Fatal(err)
	}
	gitReadsBefore := len(base.ReadsWhen["git"].Subcommands)

	merged, _ := base.Merge(ShellRules{
		AlwaysWrite: []string{"mycleanup"},
		ReadsWhen: map[string]ProgramRules{
			"git":     {Subcommands: []string{"my-read-alias"}},
			"bazel":   {Subcommands: []string{"query", "info"}},
			"ansible": {Subcommands: []string{"--version"}},
		},
	})
	if err := merged.Compile(); err != nil {
		t.Fatal(err)
	}

	if got := len(merged.ReadsWhen["git"].Subcommands); got != gitReadsBefore+1 {
		t.Errorf("git read subcommands = %d, want %d — built-ins were discarded", got, gitReadsBefore+1)
	}
	if !contains(merged.AlwaysWrite, "mycleanup") || !contains(merged.AlwaysWrite, "rm") {
		t.Error("always_write did not merge additively")
	}
	// A newly covered program gets the fail-safe default: unknown invocations write.
	if got := merged.ShellFloor(`{"command":"bazel query //..."}`); got.Kind == "write" {
		t.Errorf("bazel query was floored: %+v", got)
	}
	if got := merged.ShellFloor(`{"command":"bazel build //..."}`); got.Kind != "write" {
		t.Errorf("bazel build was not floored: %+v — unknown subcommands must fail safe", got)
	}
}

// Weakening is allowed but must be reported: silently dropping sudo from the
// always-write list is exactly the kind of thing to be reminded of.
func TestMerge_NeverWriteWeakensAndReports(t *testing.T) {
	base, err := DefaultShellRules()
	if err != nil {
		t.Fatal(err)
	}

	merged, notices := base.Merge(ShellRules{NeverWrite: []string{"touch"}})
	if contains(merged.AlwaysWrite, "touch") {
		t.Error("never_write did not remove touch")
	}
	if contains(merged.AlwaysWrite, "rm") == false {
		t.Error("never_write removed more than it was asked to")
	}

	var reported bool
	for _, n := range notices {
		if strings.Contains(n, "touch") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("notices = %v, want the weakening reported", notices)
	}
}

func TestMerge_InvalidPatternIsAnError(t *testing.T) {
	base, _ := DefaultShellRules()
	merged, _ := base.Merge(ShellRules{
		ReadsWhen: map[string]ProgramRules{"git": {Patterns: []string{"^(unclosed"}}},
	})
	if err := merged.Compile(); err == nil {
		t.Fatal("expected an error for an invalid pattern; a rule the user believes is in effect must not be skipped silently")
	}
}

// Patterns describe reads. If they described writes, anything unmatched would
// fall through as a read and an unanticipated command would run silently.
func TestReadPatterns_OnlyWidenWhatIsAllowed(t *testing.T) {
	base, err := DefaultShellRules()
	if err != nil {
		t.Fatal(err)
	}
	// `git branch` lists, so a pattern admits it; `git branch -d x` does not match.
	if got := base.ShellFloor(`{"command":"git branch -a"}`); got.Kind == "write" {
		t.Error("git branch -a was floored despite a read pattern")
	}
	if got := base.ShellFloor(`{"command":"git branch -d old"}`); got.Kind != "write" {
		t.Error("git branch -d slipped through a read pattern")
	}
}
