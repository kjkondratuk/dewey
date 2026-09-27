package compaction

import (
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"
	"github.com/pkoukk/tiktoken-go"
)

// perMessageOverheadTokens approximates the fixed per-message formatting
// cost (role markers, message boundaries) that a raw content-only token
// count misses.
const perMessageOverheadTokens = 4

var (
	encOnce sync.Once
	enc     *tiktoken.Tiktoken
	encErr  error
)

func getEncoder() (*tiktoken.Tiktoken, error) {
	encOnce.Do(func() {
		enc, encErr = tiktoken.GetEncoding("cl100k_base")
	})
	return enc, encErr
}

// EstimateTokens approximates the token count of a message history using a
// BPE tokenizer (cl100k_base encoding). This is an approximation — models
// other than OpenAI's use different vocabularies — but is far more
// consistent than a raw character-count heuristic. Used as a fallback/delta
// estimator alongside provider-reported usage (see internal/agent).
func EstimateTokens(messages []*schema.Message) int {
	encoder, err := getEncoder()
	if err != nil {
		// GetEncoding only fails if the BPE vocab can't be loaded (e.g. no
		// cache and no network); returning 0 here means shouldCompact
		// (internal/agent) will never trigger compaction rather than always
		// triggering it — the safer failure direction for an unattended
		// agent loop.
		return 0
	}

	total := 0
	for _, msg := range messages {
		total += perMessageOverheadTokens
		total += len(encoder.EncodeOrdinary(messageText(msg)))
	}
	return total
}

func messageText(msg *schema.Message) string {
	var b strings.Builder
	b.WriteString(string(msg.Role))
	b.WriteString(": ")
	b.WriteString(msg.Content)
	for _, tc := range msg.ToolCalls {
		b.WriteString(" ")
		b.WriteString(tc.Function.Name)
		b.WriteString(" ")
		b.WriteString(tc.Function.Arguments)
	}
	if msg.ToolName != "" {
		b.WriteString(" ")
		b.WriteString(msg.ToolName)
	}
	return b.String()
}
