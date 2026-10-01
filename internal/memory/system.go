package memory

import (
	"context"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// SystemPrompt composes the base instructions with the user's applicable
// memories. It satisfies agent.SystemPromptProvider.
type SystemPrompt struct {
	base string
	// provider is nil when memory is disabled, which must still yield a usable
	// system prompt rather than no system message at all.
	provider *Provider
}

func NewSystemPrompt(base string, provider *Provider) *SystemPrompt {
	return &SystemPrompt{base: base, provider: provider}
}

func (s *SystemPrompt) System(ctx context.Context) (*schema.Message, error) {
	content := s.base
	if s.provider != nil {
		block, err := s.provider.Block(ctx)
		if err != nil {
			return nil, err
		}
		// An empty block means nothing applies here; emitting the heading
		// anyway would hand the model an empty list to fill in.
		if block != "" {
			var b strings.Builder
			b.WriteString(content)
			b.WriteString("\n\nWhat you know about this user:\n")
			b.WriteString(block)
			content = b.String()
		}
	}
	return &schema.Message{Role: schema.System, Content: content}, nil
}
