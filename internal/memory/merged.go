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

func (s *MergedStore) Load(ctx context.Context) (LoadResult, error) {
	res, err := s.global.Load(ctx)
	if err != nil {
		return LoadResult{}, err
	}
	if s.project == nil {
		return res, nil
	}

	projectRes, err := s.project.Load(ctx)
	if err != nil {
		return LoadResult{}, err
	}

	index := make(map[string]int, len(res.Memories))
	for i, m := range res.Memories {
		index[m.ID] = i
	}
	for _, m := range projectRes.Memories {
		if i, ok := index[m.ID]; ok {
			res.Memories[i] = m // documented override, not a conflict
			continue
		}
		index[m.ID] = len(res.Memories)
		res.Memories = append(res.Memories, m)
	}
	res.Skipped = append(res.Skipped, projectRes.Skipped...)
	return res, nil
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
