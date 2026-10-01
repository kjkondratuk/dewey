package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// failingStore makes the load path fail, so the system prompt's handling of an
// unreadable memory directory can be observed.
type failingStore struct{ err error }

func (s *failingStore) Load(ctx context.Context) (LoadResult, error) { return LoadResult{}, s.err }
func (s *failingStore) Save(ctx context.Context, m Memory) error     { return s.err }
func (s *failingStore) Revision(ctx context.Context) string          { return "r" }

func TestSystemPrompt_CombinesBaseAndMemories(t *testing.T) {
	store := &countingStore{memories: []Memory{{ID: "a", Text: "Prefers mise."}}, rev: "r"}
	sp := NewSystemPrompt("BASE RULES", NewProvider(store, TagRetriever{}, Context{}, 4000))

	msg, err := sp.System(context.Background())
	if err != nil {
		t.Fatalf("System() error = %v", err)
	}
	if msg.Role != schema.System {
		t.Errorf("Role = %v, want %v: the model treats any other role as conversation", msg.Role, schema.System)
	}
	if !strings.Contains(msg.Content, "BASE RULES") {
		t.Errorf("Content = %q, want the base prompt: without it the agent has no instructions at all", msg.Content)
	}
	if !strings.Contains(msg.Content, "Prefers mise.") {
		t.Errorf("Content = %q, want the memory: a profile the model never sees cannot influence it", msg.Content)
	}
}

func TestSystemPrompt_NoMemoriesYieldsBaseOnly(t *testing.T) {
	sp := NewSystemPrompt("BASE RULES", NewProvider(&countingStore{rev: "r"}, TagRetriever{}, Context{}, 4000))

	msg, err := sp.System(context.Background())
	if err != nil {
		t.Fatalf("System() error = %v", err)
	}
	// An empty "what you know about this user" heading would invite the model
	// to invent entries under it, so nothing at all is the right output.
	if msg.Content != "BASE RULES" {
		t.Errorf("Content = %q, want the base prompt with no memory section", msg.Content)
	}
}

func TestSystemPrompt_NilProviderYieldsBaseOnly(t *testing.T) {
	msg, err := NewSystemPrompt("BASE RULES", nil).System(context.Background())
	if err != nil {
		t.Fatalf("System() error = %v", err)
	}
	if msg.Content != "BASE RULES" {
		t.Errorf("Content = %q, want the base prompt: memory being disabled must not disable the agent", msg.Content)
	}
}

func TestSystemPrompt_PropagatesLoadError(t *testing.T) {
	want := errors.New("memory dir unreadable")
	sp := NewSystemPrompt("BASE RULES", NewProvider(&failingStore{err: want}, TagRetriever{}, Context{}, 4000))

	// Surfacing the failure beats sending a silently memory-less prompt: the
	// user would otherwise see the agent quietly forget everything it knows.
	if _, err := sp.System(context.Background()); !errors.Is(err, want) {
		t.Errorf("System() error = %v, want %v", err, want)
	}
}
