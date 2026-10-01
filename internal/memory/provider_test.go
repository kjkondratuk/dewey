package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// countingStore reports how many times it was actually read, so the cache can
// be observed rather than assumed.
type countingStore struct {
	memories []Memory
	rev      string
	loads    int
}

func (s *countingStore) Load(ctx context.Context) (LoadResult, error) {
	s.loads++
	return LoadResult{Memories: s.memories}, nil
}
func (s *countingStore) Save(ctx context.Context, m Memory) error { return nil }
func (s *countingStore) Revision(ctx context.Context) string      { return s.rev }

func TestProvider_BlockIncludesSelectedMemories(t *testing.T) {
	store := &countingStore{memories: []Memory{
		{ID: "a", Text: "Prefers table-driven tests."},
		{ID: "b", Org: "other", Text: "Should not appear."},
	}}
	p := NewProvider(store, TagRetriever{}, Context{Org: "acme"}, 4000)

	block, err := p.Block(context.Background())
	if err != nil {
		t.Fatalf("Block() error = %v", err)
	}
	if !strings.Contains(block, "Prefers table-driven tests.") {
		t.Errorf("Block() = %q, want the applicable memory", block)
	}
	if strings.Contains(block, "Should not appear.") {
		t.Errorf("Block() = %q, leaked a memory from another org", block)
	}
}

func TestProvider_EmptySelectionYieldsEmptyBlock(t *testing.T) {
	p := NewProvider(&countingStore{}, TagRetriever{}, Context{}, 4000)
	block, err := p.Block(context.Background())
	if err != nil {
		t.Fatalf("Block() error = %v", err)
	}
	if block != "" {
		t.Errorf("Block() = %q, want empty when nothing applies", block)
	}
}

func TestProvider_CachesUntilRevisionChanges(t *testing.T) {
	store := &countingStore{memories: []Memory{{ID: "a", Text: "A."}}, rev: "r1"}
	p := NewProvider(store, TagRetriever{}, Context{}, 4000)

	for i := 0; i < 3; i++ {
		if _, err := p.Block(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if store.loads != 1 {
		t.Errorf("store loaded %d times, want 1 — the cache isn't holding", store.loads)
	}

	store.rev = "r2"
	if _, err := p.Block(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.loads != 2 {
		t.Errorf("store loaded %d times, want 2 — a revision change must force a reload", store.loads)
	}
}

func TestProvider_BudgetDropsOldestFirstAndReportsIt(t *testing.T) {
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &countingStore{memories: []Memory{
		{ID: "old", Text: strings.Repeat("old fact ", 40), Created: old},
		{ID: "recent", Text: strings.Repeat("recent fact ", 40), Created: recent},
	}}
	// Each of these memories costs 82 tokens, so a 100-token budget has room
	// for exactly one and forces a choice between them.
	p := NewProvider(store, TagRetriever{}, Context{}, 100)

	block, err := p.Block(context.Background())
	if err != nil {
		t.Fatalf("Block() error = %v", err)
	}
	if strings.Contains(block, "old fact") {
		t.Errorf("Block() = %q, want the oldest memory dropped first", block)
	}
	if !strings.Contains(block, "recent fact") {
		t.Errorf("Block() = %q, want the newest memory kept", block)
	}
	if len(p.Notices()) == 0 {
		t.Error("dropping memories must be reported, not silent")
	}
}

func TestProvider_SkipNoticesSurface(t *testing.T) {
	store := &skipStore{}
	p := NewProvider(store, TagRetriever{}, Context{}, 4000)
	if _, err := p.Block(context.Background()); err != nil {
		t.Fatal(err)
	}
	notices := p.Notices()
	if len(notices) != 1 || !strings.Contains(notices[0], "bad.md") {
		t.Errorf("Notices() = %v, want the skipped file reported", notices)
	}
}

type skipStore struct{}

func (skipStore) Load(ctx context.Context) (LoadResult, error) {
	return LoadResult{Skipped: []SkipNotice{{File: "bad.md", Reason: "invalid frontmatter"}}}, nil
}
func (skipStore) Save(ctx context.Context, m Memory) error { return nil }
func (skipStore) Revision(ctx context.Context) string      { return "r" }
