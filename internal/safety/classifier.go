package safety

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/kjkondratuk/dewey/prompts"
)

// Classifier assigns a Classification to a resolved tool call. On error, the
// returned Classification must itself be a safe upper bound (fail-safe) —
// callers may use it directly without special-casing err.
type Classifier interface {
	Classify(ctx context.Context, toolName, toolDesc, argsJSON string) (Classification, error)
}

// classifierSystemPrompt lives in prompts/classifier.md.
var classifierSystemPrompt = prompts.Classifier

// shellToolName is the tool the deterministic floor understands. The floor is
// shell-specific by nature, so it is applied by name rather than pretending to
// be generic.
const shellToolName = "run_shell_command"

// failSafeClassification is returned whenever classification cannot be
// trusted — a model error, a malformed response, or an unrecognized field
// value. It must never be downgraded.
var failSafeClassification = Classification{Kind: "write", Risk: Dangerous}

// llmClassifier calls a chat model to classify each tool call, in a message
// exchange isolated from any ongoing conversation history.
type llmClassifier struct {
	model model.BaseChatModel
}

// NewLLMClassifier builds a Classifier backed by m. m should be a small/fast
// model — classification runs once per tool call.
func NewLLMClassifier(m model.BaseChatModel) Classifier {
	return &llmClassifier{model: m}
}

func (c *llmClassifier) Classify(ctx context.Context, toolName, toolDesc, argsJSON string) (Classification, error) {
	messages := []*schema.Message{
		{Role: schema.System, Content: classifierSystemPrompt},
		{
			Role: schema.User,
			Content: fmt.Sprintf("Tool: %s\nDescription: %s\nArguments: %s",
				toolName, toolDesc, argsJSON),
		},
	}

	resp, err := c.model.Generate(ctx, messages)
	if err != nil {
		return failSafeClassification, fmt.Errorf("classifier model call failed: %w", err)
	}

	classification, err := parseClassification(resp.Content)
	if err != nil {
		return failSafeClassification, err
	}
	return raiseToFloor(classification, toolName, argsJSON), nil
}

// raiseToFloor applies a deterministic minimum to a model-produced
// classification. The floor can only increase what gets confirmed: the model
// may escalate beyond it, never relax below it, because a language model
// reasoning about consequences is not a sound basis for the only gate in front
// of a tool that can run arbitrary commands.
func raiseToFloor(c Classification, toolName, argsJSON string) Classification {
	if toolName != shellToolName {
		return c
	}

	floor := ShellFloor(argsJSON)
	if floor.Kind == "write" {
		c.Kind = "write"
	}
	if floor.Risk > c.Risk {
		c.Risk = floor.Risk
	}
	return c
}

func parseClassification(raw string) (Classification, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start == -1 || end == -1 || end < start {
		return Classification{}, fmt.Errorf("no JSON object found in classifier response: %q", raw)
	}

	var parsed struct {
		Kind    string `json:"kind"`
		Risk    string `json:"risk"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &parsed); err != nil {
		return Classification{}, fmt.Errorf("invalid classifier JSON: %w", err)
	}

	if parsed.Kind != "read" && parsed.Kind != "write" {
		return Classification{}, fmt.Errorf("unrecognized classifier kind %q", parsed.Kind)
	}

	risk, err := ParseRiskTier(parsed.Risk)
	if err != nil {
		return Classification{}, fmt.Errorf("unrecognized classifier risk: %w", err)
	}

	// Summary is deliberately not validated: it is cosmetic, so a model that
	// omits it should still produce a usable classification rather than being
	// forced down the fail-safe path.
	return Classification{
		Kind:    parsed.Kind,
		Risk:    risk,
		Summary: strings.TrimSpace(parsed.Summary),
	}, nil
}
