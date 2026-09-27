package memory

import (
	"context"
	"testing"
)

func TestMergedStore_ProjectOverridesGlobalByID(t *testing.T) {
	globalDir, projectDir := t.TempDir(), t.TempDir()
	writeMemFile(t, globalDir, "style.md", "---\nid: style\n---\nGlobal style.\n")
	writeMemFile(t, globalDir, "only-global.md", "---\nid: only-global\n---\nGlobal only.\n")
	writeMemFile(t, projectDir, "style.md", "---\nid: style\n---\nProject style.\n")

	ms := NewMergedStore(NewFileStore(globalDir), NewFileStore(projectDir))
	res, err := ms.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Memories) != 2 {
		t.Fatalf("loaded %d memories, want 2", len(res.Memories))
	}

	byID := map[string]string{}
	for _, m := range res.Memories {
		byID[m.ID] = m.Text
	}
	if byID["style"] != "Project style." {
		t.Errorf("style = %q, want the project copy to win", byID["style"])
	}
	if byID["only-global"] != "Global only." {
		t.Errorf("only-global = %q, want it preserved", byID["only-global"])
	}
}

func TestMergedStore_NilProjectStoreIsFine(t *testing.T) {
	globalDir := t.TempDir()
	writeMemFile(t, globalDir, "a.md", "---\nid: a\n---\nA.\n")

	res, err := NewMergedStore(NewFileStore(globalDir), nil).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Memories) != 1 {
		t.Errorf("loaded %d memories, want 1", len(res.Memories))
	}
}

func TestMergedStore_SavesToGlobal(t *testing.T) {
	globalDir, projectDir := t.TempDir(), t.TempDir()
	global := NewFileStore(globalDir)
	project := NewFileStore(projectDir)

	if err := NewMergedStore(global, project).Save(context.Background(), Memory{ID: "new", Text: "New."}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if res, _ := global.Load(context.Background()); len(res.Memories) != 1 {
		t.Errorf("global has %d memories, want 1", len(res.Memories))
	}
	if res, _ := project.Load(context.Background()); len(res.Memories) != 0 {
		t.Errorf("project has %d memories, want 0 — agent writes belong in the global store", len(res.Memories))
	}
}

func TestMergedStore_RevisionTracksBothStores(t *testing.T) {
	global, project := NewFileStore(t.TempDir()), NewFileStore(t.TempDir())
	ms := NewMergedStore(global, project)

	before := ms.Revision(context.Background())
	if err := project.Save(context.Background(), Memory{ID: "p", Text: "P."}); err != nil {
		t.Fatal(err)
	}
	if after := ms.Revision(context.Background()); after == before {
		t.Error("Revision unchanged after a project-store write")
	}
}

func TestMergedStore_SkipNoticesFromBothStoresAreKept(t *testing.T) {
	globalDir, projectDir := t.TempDir(), t.TempDir()
	writeMemFile(t, globalDir, "bad1.md", "---\nid: [bad\n---\nx\n")
	writeMemFile(t, projectDir, "bad2.md", "---\nid: [bad\n---\nx\n")

	res, _ := NewMergedStore(NewFileStore(globalDir), NewFileStore(projectDir)).Load(context.Background())
	if len(res.Skipped) != 2 {
		t.Errorf("Skipped = %+v, want notices from both stores", res.Skipped)
	}
}
