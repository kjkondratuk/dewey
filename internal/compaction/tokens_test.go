package compaction

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestEstimateTokens_PositiveAndProportionalToContent(t *testing.T) {
	short := []*schema.Message{{Role: schema.User, Content: "hi"}}
	long := []*schema.Message{{Role: schema.User, Content: strings.Repeat("word ", 200)}}

	shortCount := EstimateTokens(short)
	longCount := EstimateTokens(long)

	if shortCount <= 0 {
		t.Fatalf("EstimateTokens(short) = %d, want > 0", shortCount)
	}
	if longCount <= shortCount {
		t.Errorf("EstimateTokens(long) = %d, want > EstimateTokens(short) = %d", longCount, shortCount)
	}
}

func TestEstimateTokens_EmptyHistory(t *testing.T) {
	if got := EstimateTokens(nil); got != 0 {
		t.Errorf("EstimateTokens(nil) = %d, want 0", got)
	}
}

func TestEstimateTokens_CountsToolCallsAndResults(t *testing.T) {
	withToolCall := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			Function: schema.FunctionCall{Name: "Bash", Arguments: `{"command":"ls -la /some/long/path/here"}`},
		}}},
	}
	withoutToolCall := []*schema.Message{{Role: schema.Assistant}}

	if EstimateTokens(withToolCall) <= EstimateTokens(withoutToolCall) {
		t.Error("expected tool call name/arguments to contribute to the token estimate")
	}
}

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
