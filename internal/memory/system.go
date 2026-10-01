package memory

import (
	"context"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// SystemPrompt composes the base instructions with the memory instructions and
// the user's applicable memories. It satisfies agent.SystemPromptProvider.
type SystemPrompt struct {
	base string
	// memoryInstructions explains the remember tool. It is sent only when
	// provider is non-nil: with memory disabled the tool is never bound, and
	// describing a tool the model cannot call invites it to promise to
	// remember something, or to attempt a call that does not exist.
	memoryInstructions string
	// provider is nil when memory is disabled, which must still yield a usable
	// system prompt rather than no system message at all.
	provider *Provider
}

func NewSystemPrompt(base, memoryInstructions string, provider *Provider) *SystemPrompt {
	return &SystemPrompt{base: base, memoryInstructions: memoryInstructions, provider: provider}
}

func (s *SystemPrompt) System(ctx context.Context) (*schema.Message, error) {
	var b strings.Builder
	b.WriteString(s.base)

	if s.provider != nil {
		if s.memoryInstructions != "" {
			b.WriteString("\n\n")
			b.WriteString(s.memoryInstructions)
		}

		block, err := s.provider.Block(ctx)
		if err != nil {
			return nil, err
		}
		// An empty block means nothing applies here; emitting the heading
		// anyway would hand the model an empty list to fill in.
		if block != "" {
			b.WriteString("\n\nWhat you know about this user:\n")
			b.WriteString(block)
		}
	}

	return &schema.Message{Role: schema.System, Content: b.String()}, nil
}

// Notices reports anything the user should know about the last load. It exists
// so the agent can surface problems that arise mid-session: the startup report
// only covers the first load, but a memory saved later can evict another for
// budget, and a file can become malformed while a session is running.
func (s *SystemPrompt) Notices() []string {
	if s.provider == nil {
		return nil
	}
	return s.provider.Notices()
}
