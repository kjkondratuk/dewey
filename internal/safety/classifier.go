package safety

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Classifier assigns a Classification to a resolved tool call. On error, the
// returned Classification must itself be a safe upper bound (fail-safe) —
// callers may use it directly without special-casing err.
type Classifier interface {
	Classify(ctx context.Context, toolName, toolDesc, argsJSON string) (Classification, error)
}

const classifierSystemPrompt = `You are a safety classifier for an autonomous agent's tool calls.
Given a tool name, its description, and the exact arguments it is about to be called with, decide:
- "kind": "read" if the call only observes state, "write" if it can change state (files, network resources, external systems).
- "risk": "safe" for read-only or clearly inconsequential actions, "moderate" for reversible writes with limited blast radius, "dangerous" for irreversible, destructive, or wide-blast-radius actions.
Respond with ONLY a single JSON object of the form {"kind":"read"|"write","risk":"safe"|"moderate"|"dangerous"}. No other text, no markdown fences.`

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
	return classification, nil
}

func parseClassification(raw string) (Classification, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start == -1 || end == -1 || end < start {
		return Classification{}, fmt.Errorf("no JSON object found in classifier response: %q", raw)
	}

	var parsed struct {
		Kind string `json:"kind"`
		Risk string `json:"risk"`
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

	return Classification{Kind: parsed.Kind, Risk: risk}, nil
}
