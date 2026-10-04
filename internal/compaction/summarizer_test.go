package compaction

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type fakeGenerateModel struct {
	content string
	err     error
	lastReq []*schema.Message
}

func (f *fakeGenerateModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	f.lastReq = in
	if f.err != nil {
		return nil, f.err
	}
	return &schema.Message{Role: schema.Assistant, Content: f.content}, nil
}

func (f *fakeGenerateModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	panic("not used in these tests")
}

func TestLLMSummarizer_ReturnsModelText(t *testing.T) {
	fake := &fakeGenerateModel{content: "summary text"}
	s := NewLLMSummarizer(fake)

	got, err := s.Summarize(context.Background(), []*schema.Message{{Role: schema.User, Content: "hello"}})
	if err != nil {
		t.Fatalf("Summarize() error = %v", err)
	}
	if got != "summary text" {
		t.Errorf("Summarize() = %q, want %q", got, "summary text")
	}
}

func TestLLMSummarizer_IsolatedFromInput(t *testing.T) {
	fake := &fakeGenerateModel{content: "summary"}
	s := NewLLMSummarizer(fake)

	history := []*schema.Message{{Role: schema.User, Content: "hello"}}
	if _, err := s.Summarize(context.Background(), history); err != nil {
		t.Fatalf("Summarize() error = %v", err)
	}

	if len(history) != 1 {
		t.Fatalf("input history was mutated: len = %d, want 1", len(history))
	}
	if len(fake.lastReq) != 2 {
		t.Fatalf("model received %d messages, want 2 (system + user transcript)", len(fake.lastReq))
	}
	if fake.lastReq[0].Role != schema.System {
		t.Errorf("first message role = %v, want schema.System", fake.lastReq[0].Role)
	}
	if !strings.Contains(fake.lastReq[1].Content, "hello") {
		t.Errorf("transcript missing original content: %q", fake.lastReq[1].Content)
	}
}

func TestLLMSummarizer_ModelErrorPropagates(t *testing.T) {
	fake := &fakeGenerateModel{err: errors.New("connection refused")}
	s := NewLLMSummarizer(fake)

	_, err := s.Summarize(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestLLMSummarizer_TranscribesToolCallsAndResults(t *testing.T) {
	fake := &fakeGenerateModel{content: "summary"}
	s := NewLLMSummarizer(fake)

	history := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			Function: schema.FunctionCall{Name: "Bash", Arguments: `{"command":"ls"}`},
		}}},
		schema.ToolMessage("file1.txt\nfile2.txt", "call-1"),
	}

	if _, err := s.Summarize(context.Background(), history); err != nil {
		t.Fatalf("Summarize() error = %v", err)
	}

	transcript := fake.lastReq[1].Content
	if !strings.Contains(transcript, "Bash") {
		t.Errorf("transcript missing tool call name: %q", transcript)
	}
	if !strings.Contains(transcript, "file1.txt") {
		t.Errorf("transcript missing tool result: %q", transcript)
	}
}

func TestLLMSummarizer_MixedHistoryOrdering(t *testing.T) {
	fake := &fakeGenerateModel{content: "summary"}
	s := NewLLMSummarizer(fake)

	history := []*schema.Message{
		{Role: schema.User, Content: "please list files"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			Function: schema.FunctionCall{Name: "Bash", Arguments: `{"command":"ls"}`},
		}}},
		schema.ToolMessage("file1.txt\nfile2.txt", "call-1"),
		{Role: schema.Assistant, Content: "Here are the files."},
	}

	if _, err := s.Summarize(context.Background(), history); err != nil {
		t.Fatalf("Summarize() error = %v", err)
	}

	transcript := fake.lastReq[1].Content
	wantOrder := []string{"please list files", "Bash", "file1.txt", "Here are the files."}
	lastIdx := -1
	for _, want := range wantOrder {
		idx := strings.Index(transcript, want)
		if idx == -1 {
			t.Fatalf("transcript missing %q entirely:\n%s", want, transcript)
		}
		if idx < lastIdx {
			t.Errorf("expected %q to appear after index %d, but found at %d (out of order):\n%s", want, lastIdx, idx, transcript)
		}
		lastIdx = idx
	}
}
