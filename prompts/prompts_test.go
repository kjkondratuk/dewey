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
		"Classifier": Classifier,
		"Summarizer": Summarizer,
		"ToolDenied": ToolDenied,
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

// The whole point of this wording is that a denial cannot be mistaken for a
// capability failure, which is what an "error:" prefix would signal.
func TestToolDeniedDoesNotReadAsAnError(t *testing.T) {
	if strings.HasPrefix(ToolDenied, "error:") {
		t.Errorf("ToolDenied looks like a tool failure: %q", ToolDenied)
	}
}
