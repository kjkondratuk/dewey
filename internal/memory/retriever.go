package memory

// Retriever chooses which memories apply to the current context.
//
// This interface is the seam for smarter retrieval. Tag matching is enough
// while the corpus is small enough to inject wholesale; an embedding-backed
// implementation can replace TagRetriever without the store, the tool, or the
// prompt assembly changing.
type Retriever interface {
	Select(memories []Memory, mc Context) []Memory
}

// TagRetriever selects by exact org match and tag intersection.
type TagRetriever struct{}

var _ Retriever = TagRetriever{}

func (TagRetriever) Select(memories []Memory, mc Context) []Memory {
	var out []Memory
	for _, m := range memories {
		// Org gates: a memory scoped to an organization applies only to that
		// organization's work, and never when the org is unknown. Tags only
		// filter, so a topical match can never override this.
		if m.Org != "" && m.Org != mc.Org {
			continue
		}
		if len(m.Tags) > 0 && !intersects(m.Tags, mc.Tags) {
			continue
		}
		out = append(out, m)
	}
	return out
}

func intersects(a, b []string) bool {
	for _, x := range a {
		if contains(b, x) {
			return true
		}
	}
	return false
}
