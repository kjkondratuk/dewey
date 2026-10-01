package memory

import "context"

// MergedStore layers a project store over a global one, the same way config
// resolves project settings over global ones. Writes go to the global store:
// project memories are meant to be authored and committed deliberately, not
// accumulated by the agent.
type MergedStore struct {
	global  Store
	project Store // nil when running outside a project
}

func NewMergedStore(global, project Store) *MergedStore {
	return &MergedStore{global: global, project: project}
}

// Load merges the two stores: every project memory, plus every global memory
// whose id no project memory claims.
//
// Stating the rule that way matters. Patching global entries in place instead
// only replaced the *last* duplicate of a given id, so an instruction the
// project meant to override could still reach the prompt alongside its
// replacement. It also made duplicate ids behave differently depending on
// which store they were in, even though production always reads through here.
//
// A store that fails to load degrades to a skip notice rather than an error:
// losing the global profile because a project directory happens to be
// unreadable would be a worse outcome than running without the project's
// additions, and the rest of this package already prefers reporting over
// failing.
func (s *MergedStore) Load(ctx context.Context) (LoadResult, error) {
	global := s.loadOne(ctx, s.global, "global")
	if s.project == nil {
		return global, nil
	}
	project := s.loadOne(ctx, s.project, "project")

	claimed := make(map[string]bool, len(project.Memories))
	for _, m := range project.Memories {
		claimed[m.ID] = true
	}

	res := LoadResult{
		Memories: make([]Memory, 0, len(global.Memories)+len(project.Memories)),
		Skipped:  append(global.Skipped, project.Skipped...),
	}
	for _, m := range global.Memories {
		if !claimed[m.ID] {
			res.Memories = append(res.Memories, m)
		}
	}
	res.Memories = append(res.Memories, project.Memories...)
	return res, nil
}

func (s *MergedStore) loadOne(ctx context.Context, store Store, label string) LoadResult {
	res, err := store.Load(ctx)
	if err != nil {
		return LoadResult{Skipped: []SkipNotice{{
			File:   label + " memory store",
			Reason: err.Error(),
		}}}
	}
	return res
}

func (s *MergedStore) Save(ctx context.Context, m Memory) error {
	return s.global.Save(ctx, m)
}

func (s *MergedStore) Revision(ctx context.Context) string {
	rev := s.global.Revision(ctx)
	if s.project != nil {
		rev += "|" + s.project.Revision(ctx)
	}
	return rev
}
