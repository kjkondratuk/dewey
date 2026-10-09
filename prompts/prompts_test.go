package prompts

import (
	"strings"
	"testing"
)

// An embed that resolves to an empty string still compiles, so without this
// the first sign of a renamed or emptied prompt file would be a model quietly
// behaving worse.
func TestPromptsAreNonEmptyAndTrimmed(t *testing.T) {
	for name, got := range map[string]string{
		"Classifier":   Classifier,
		"Summarizer":   Summarizer,
		"ToolDenied":   ToolDenied,
		"SystemBase":   SystemBase,
		"SystemMemory": SystemMemory,
	} {
		if got == "" {
			t.Errorf("%s is empty — is its markdown file missing?", name)
			continue
		}
		if got != strings.TrimSpace(got) {
			t.Errorf("%s has untrimmed surrounding whitespace", name)
		}
	}
}

// internal/safety parses the classifier's reply as JSON and fails safe when it
// can't, so the prompt has to keep asking for a bare JSON object.
func TestClassifierPromptStillDemandsBareJSON(t *testing.T) {
	for _, want := range []string{`"kind"`, `"risk"`, `"summary"`, "JSON"} {
		if !strings.Contains(Classifier, want) {
			t.Errorf("classifier prompt no longer mentions %s", want)
		}
	}
	if !strings.Contains(Classifier, "no markdown fences") {
		t.Error("classifier prompt dropped the no-fences instruction the parser depends on")
	}
}

// The remember tool is useless if the model is never told when to reach for
// it, and harmful if it records project trivia.
func TestSystemMemoryExplainsWhenToRemember(t *testing.T) {
	for _, want := range []string{"remember", "preference", "approval"} {
		if !strings.Contains(strings.ToLower(SystemMemory), want) {
			t.Errorf("SystemMemory does not mention %q", want)
		}
	}
}

// The memory instructions must live outside SystemBase so they can be withheld
// when memory is disabled; describing an unbound tool invites the model to
// promise something it cannot do.
func TestSystemBaseSaysNothingAboutMemory(t *testing.T) {
	if strings.Contains(strings.ToLower(SystemBase), "remember") {
		t.Error("SystemBase mentions the remember tool; it belongs in SystemMemory")
	}
}

// The whole point of this wording is that a denial cannot be mistaken for a
// capability failure, which is what an "error:" prefix would signal.
func TestToolDeniedDoesNotReadAsAnError(t *testing.T) {
	if strings.HasPrefix(ToolDenied, "error:") {
		t.Errorf("ToolDenied looks like a tool failure: %q", ToolDenied)
	}
}

// dewey has Read/Write/Edit/Grep/Glob tools alongside Bash; a prompt that
// only talks about "running shell commands" nudges the model toward Bash for
// file work it should do with the specific tools instead.
func TestSystemBaseDescribesTheSpecificToolsNotJustShell(t *testing.T) {
	lower := strings.ToLower(SystemBase)
	for _, want := range []string{"read", "edit", "search"} {
		if !strings.Contains(lower, want) {
			t.Errorf("SystemBase does not mention %q; it should describe reading, editing, and searching, not just running commands", want)
		}
	}
}

// The model has no way to recover from its own repetition by generating more
// output; the one documented failure (a model streaming the same text until
// the user killed it) was a model that never noticed it was stuck. The
// prompt must tell it to stop and ask instead.
func TestSystemBaseInstructsStoppingOnSelfRepetition(t *testing.T) {
	lower := strings.ToLower(SystemBase)
	for _, want := range []string{"repeat", "stop", "ask"} {
		if !strings.Contains(lower, want) {
			t.Errorf("SystemBase does not mention %q; it should instruct the model to notice repetition and stop to ask for direction", want)
		}
	}
}

// Bash refuses commands a specific tool covers (see safety.PreferredTool). A
// model that does not know the refusal is coming spends a turn discovering it,
// and may read it as a broken capability rather than a redirect.
func TestSystemBaseWarnsThatBashRefusesCoveredCommands(t *testing.T) {
	lower := strings.ToLower(SystemBase)
	for _, want := range []string{"refuse", "bash"} {
		if !strings.Contains(lower, want) {
			t.Errorf("SystemBase does not mention %q; it should tell the model Bash refuses what another tool covers", want)
		}
	}
}
