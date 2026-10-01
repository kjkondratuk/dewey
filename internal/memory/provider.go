package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/kjkondratuk/dewey/internal/compaction"
)

// Provider turns the stores into the memory block that goes in the system
// prompt, reloading only when the underlying stores actually change.
type Provider struct {
	store     Store
	retriever Retriever
	mc        Context
	maxTokens int

	mu      sync.Mutex
	haveRev bool
	lastRev string
	block   string
	notices []string
}

func NewProvider(store Store, retriever Retriever, mc Context, maxTokens int) *Provider {
	return &Provider{store: store, retriever: retriever, mc: mc, maxTokens: maxTokens}
}

// Block returns the rendered memory block, or "" when nothing applies.
func (p *Provider) Block(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	rev := p.store.Revision(ctx)
	if p.haveRev && rev == p.lastRev {
		return p.block, nil
	}

	res, err := p.store.Load(ctx)
	if err != nil {
		return "", err
	}

	notices := make([]string, 0, len(res.Skipped))
	for _, s := range res.Skipped {
		notices = append(notices, fmt.Sprintf("skipped memory %s: %s", s.File, s.Reason))
	}

	selected := p.retriever.Select(res.Memories, p.mc)
	selected, dropped := applyBudget(selected, p.maxTokens)
	if dropped > 0 {
		notices = append(notices, fmt.Sprintf(
			"memory budget of %d tokens exceeded; %d older memories omitted", p.maxTokens, dropped))
	}

	p.block = render(selected)
	p.notices = notices
	p.lastRev = rev
	p.haveRev = true
	return p.block, nil
}

// Notices reports anything the user should know about the last load: files
// that couldn't be read, memories dropped for budget. Curated content going
// missing silently would be worse than the omission itself.
func (p *Provider) Notices() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.notices...)
}

// applyBudget keeps the newest memories that fit, since a stale preference is
// likelier to have been superseded than a fresh one.
func applyBudget(memories []Memory, maxTokens int) (kept []Memory, dropped int) {
	if maxTokens <= 0 {
		return memories, 0
	}

	// Keyed by position rather than ID: two files can carry the same explicit
	// id, and keying on it would silently keep or drop both together.
	type ranked struct {
		m   Memory
		pos int
	}
	ranking := make([]ranked, len(memories))
	for i, m := range memories {
		ranking[i] = ranked{m: m, pos: i}
	}
	sort.SliceStable(ranking, func(i, j int) bool {
		return ranking[i].m.Created.After(ranking[j].m.Created)
	})

	used := 0
	keep := make([]bool, len(memories))
	for _, r := range ranking {
		cost := compaction.EstimateText("- " + r.m.Text)
		if used+cost > maxTokens {
			dropped++
			continue
		}
		used += cost
		keep[r.pos] = true
	}

	for i, m := range memories { // restore the caller's order
		if keep[i] {
			kept = append(kept, m)
		}
	}
	return kept, dropped
}

func render(memories []Memory) string {
	if len(memories) == 0 {
		return ""
	}
	var b strings.Builder
	for _, m := range memories {
		b.WriteString("- ")
		b.WriteString(strings.Join(strings.Fields(m.Text), " "))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
