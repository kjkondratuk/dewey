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
	sp := NewSystemPrompt("BASE RULES", "MEMORY RULES", NewProvider(store, TagRetriever{}, Context{}, 4000))

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

// Memory enabled but nothing applicable yet: the tool instructions still
// belong in the prompt, because the tool is bound and usable. Only the
// "what you know" list is withheld — an empty heading would invite the model
// to invent entries under it.
func TestSystemPrompt_NoMemoriesStillExplainsTheTool(t *testing.T) {
	sp := NewSystemPrompt("BASE RULES", "MEMORY RULES", NewProvider(&countingStore{rev: "r"}, TagRetriever{}, Context{}, 4000))

	msg, err := sp.System(context.Background())
	if err != nil {
		t.Fatalf("System() error = %v", err)
	}
	if !strings.Contains(msg.Content, "MEMORY RULES") {
		t.Errorf("Content = %q, want the tool instructions: the tool is bound, so the model must know about it", msg.Content)
	}
	if strings.Contains(msg.Content, "What you know about this user") {
		t.Errorf("Content = %q, want no memory heading when nothing applies", msg.Content)
	}
}

// Memory disabled: the remember tool is never bound, so describing it would
// invite the model to promise something it cannot do or call a tool that
// doesn't exist.
func TestSystemPrompt_NilProviderOmitsTheMemoryInstructions(t *testing.T) {
	msg, err := NewSystemPrompt("BASE RULES", "MEMORY RULES", nil).System(context.Background())
	if err != nil {
		t.Fatalf("System() error = %v", err)
	}
	if msg.Content != "BASE RULES" {
		t.Errorf("Content = %q, want only the base prompt — memory being disabled must neither disable the agent nor advertise an unbound tool", msg.Content)
	}
}

func TestSystemPrompt_PropagatesLoadError(t *testing.T) {
	want := errors.New("memory dir unreadable")
	sp := NewSystemPrompt("BASE RULES", "MEMORY RULES", NewProvider(&failingStore{err: want}, TagRetriever{}, Context{}, 4000))

	// Surfacing the failure beats sending a silently memory-less prompt: the
	// user would otherwise see the agent quietly forget everything it knows.
	if _, err := sp.System(context.Background()); !errors.Is(err, want) {
		t.Errorf("System() error = %v, want %v", err, want)
	}
}
