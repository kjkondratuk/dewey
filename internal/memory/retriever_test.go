package memory

import "testing"

func ids(ms []Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func TestTagRetriever_UntaggedMemoryAppliesEverywhere(t *testing.T) {
	all := []Memory{{ID: "global-pref", Text: "x"}}
	got := TagRetriever{}.Select(all, Context{Org: "acme", Tags: []string{"go"}})
	if len(got) != 1 {
		t.Errorf("Select() = %v, want the untagged memory", ids(got))
	}
}

func TestTagRetriever_OrgGatesRegardlessOfTagMatch(t *testing.T) {
	// The bug this prevents: a memory scoped to acme leaking into other work
	// because its topical tag happened to match.
	all := []Memory{{ID: "acme-go-style", Org: "acme", Tags: []string{"go"}, Text: "x"}}

	if got := (TagRetriever{}).Select(all, Context{Org: "acme", Tags: []string{"go"}}); len(got) != 1 {
		t.Errorf("Select() at acme = %v, want the memory to apply", ids(got))
	}
	if got := (TagRetriever{}).Select(all, Context{Org: "other", Tags: []string{"go"}}); len(got) != 0 {
		t.Errorf("Select() at another org = %v, want nothing — org must gate", ids(got))
	}
	if got := (TagRetriever{}).Select(all, Context{Tags: []string{"go"}}); len(got) != 0 {
		t.Errorf("Select() with unknown org = %v, want nothing — fail closed", ids(got))
	}
}

func TestTagRetriever_TagsMatchByIntersection(t *testing.T) {
	all := []Memory{
		{ID: "go-thing", Tags: []string{"go"}, Text: "x"},
		{ID: "node-thing", Tags: []string{"node"}, Text: "x"},
		{ID: "either", Tags: []string{"go", "node"}, Text: "x"},
	}
	got := TagRetriever{}.Select(all, Context{Tags: []string{"go"}})
	if len(got) != 2 || !contains(ids(got), "go-thing") || !contains(ids(got), "either") {
		t.Errorf("Select() = %v, want go-thing and either", ids(got))
	}
}

func TestTagRetriever_TaggedMemoryExcludedWhenContextHasNoTags(t *testing.T) {
	all := []Memory{{ID: "go-thing", Tags: []string{"go"}, Text: "x"}}
	if got := (TagRetriever{}).Select(all, Context{}); len(got) != 0 {
		t.Errorf("Select() = %v, want nothing in an unidentified directory", ids(got))
	}
}

func TestTagRetriever_PreservesInputOrder(t *testing.T) {
	all := []Memory{{ID: "a", Text: "x"}, {ID: "b", Text: "x"}, {ID: "c", Text: "x"}}
	got := ids(TagRetriever{}.Select(all, Context{}))
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("Select() = %v, want input order preserved", got)
	}
}
