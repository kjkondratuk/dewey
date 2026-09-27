package safety

import (
	"fmt"
	"strings"
)

// RiskTier is the safety classification assigned to a tool call.
type RiskTier int

const (
	Safe RiskTier = iota
	Moderate
	Dangerous
)

func (r RiskTier) String() string {
	switch r {
	case Safe:
		return "safe"
	case Moderate:
		return "moderate"
	case Dangerous:
		return "dangerous"
	default:
		return "dangerous"
	}
}

// ParseRiskTier parses a config/classifier string into a RiskTier. Any
// unrecognized value returns Dangerous alongside an error, so a caller that
// ignores the error still fails safe.
func ParseRiskTier(s string) (RiskTier, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "safe":
		return Safe, nil
	case "moderate":
		return Moderate, nil
	case "dangerous":
		return Dangerous, nil
	default:
		return Dangerous, fmt.Errorf("unknown risk tier %q", s)
	}
}

// Classification is the result of evaluating a single tool call.
type Classification struct {
	// Kind is "read" or "write" — informational only, it does not affect
	// confirmation gating. Empty when the classification came from a config
	// override rather than the classifier.
	Kind string
	Risk RiskTier

	// Summary is a one-sentence plain-English description of what the call
	// does, shown in the confirmation prompt so a user who can't read the
	// tool's argument syntax can still evaluate it. Cosmetic and
	// best-effort: empty when the classification came from a config
	// override, from the fail-safe path, or when the model omitted it.
	Summary string
}

// RiskOverrides pins specific tool names to a fixed RiskTier, bypassing the
// classifier entirely for that tool.
type RiskOverrides map[string]RiskTier
