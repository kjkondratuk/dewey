package compaction

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Summarizer condenses a message history into a single summary string.
type Summarizer interface {
	Summarize(ctx context.Context, messages []*schema.Message) (string, error)
}

const summarizerSystemPrompt = `You are summarizing an ongoing conversation between a user and an AI coding agent so the conversation can continue with a much shorter history.
Preserve: the user's overall goals and requests, key decisions made, important facts discovered (file paths, commands run, configuration values, error messages), the state of any in-progress work, and any open questions or next steps.
Omit conversational filler. Be thorough about anything needed to correctly continue the work, but keep the summary as concise as such thoroughness allows.`

type llmSummarizer struct {
	model model.BaseChatModel
}

// NewLLMSummarizer builds a Summarizer backed by m. m should be a small/fast
// model — summarization runs as an isolated call (a fresh message list),
// never touching or appending to the caller's conversation history.
func NewLLMSummarizer(m model.BaseChatModel) Summarizer {
	return &llmSummarizer{model: m}
}

func (s *llmSummarizer) Summarize(ctx context.Context, messages []*schema.Message) (string, error) {
	transcript := transcribe(messages)

	req := []*schema.Message{
		{Role: schema.System, Content: summarizerSystemPrompt},
		{Role: schema.User, Content: "Conversation to summarize:\n\n" + transcript},
	}

	resp, err := s.model.Generate(ctx, req)
	if err != nil {
		return "", fmt.Errorf("summarizer model call failed: %w", err)
	}
	return resp.Content, nil
}

func transcribe(messages []*schema.Message) string {
	var b strings.Builder
	for _, msg := range messages {
		switch {
		case len(msg.ToolCalls) > 0:
			for _, tc := range msg.ToolCalls {
				fmt.Fprintf(&b, "ASSISTANT called %s(%s)\n", tc.Function.Name, tc.Function.Arguments)
			}
		case msg.Role == schema.Tool:
			fmt.Fprintf(&b, "TOOL[%s]: %s\n", msg.ToolName, msg.Content)
		default:
			fmt.Fprintf(&b, "%s: %s\n", strings.ToUpper(string(msg.Role)), msg.Content)
		}
	}
	return b.String()
}
