package memory

import (
	"context"
	"os"
	"path/filepath"
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

// A project override must displace EVERY global memory with that id. Patching
// in place only replaced the last duplicate, so the instruction the project
// meant to override still reached the prompt beside its replacement.
func TestMergedStore_ProjectOverrideDisplacesAllGlobalDuplicates(t *testing.T) {
	globalDir, projectDir := t.TempDir(), t.TempDir()
	writeMemFile(t, globalDir, "a.md", "---\nid: style\n---\nGlobal A.\n")
	writeMemFile(t, globalDir, "b.md", "---\nid: style\n---\nGlobal B.\n")
	writeMemFile(t, projectDir, "style.md", "---\nid: style\n---\nProject wins.\n")

	res, err := NewMergedStore(NewFileStore(globalDir), NewFileStore(projectDir)).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Memories) != 1 {
		t.Fatalf("loaded %d memories, want 1: %+v", len(res.Memories), res.Memories)
	}
	if res.Memories[0].Text != "Project wins." {
		t.Errorf("Text = %q, want the project copy to displace both globals", res.Memories[0].Text)
	}
}

// Duplicate ids must behave the same wherever they live — production always
// reads through MergedStore, so placement must not change semantics.
func TestMergedStore_DuplicateIDsInOneStoreAllSurvive(t *testing.T) {
	projectDir := t.TempDir()
	writeMemFile(t, projectDir, "p1.md", "---\nid: dup\n---\nProject one.\n")
	writeMemFile(t, projectDir, "p2.md", "---\nid: dup\n---\nProject two.\n")

	viaMerged, err := NewMergedStore(NewFileStore(t.TempDir()), NewFileStore(projectDir)).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	viaFile, err := NewFileStore(projectDir).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(viaMerged.Memories) != len(viaFile.Memories) {
		t.Errorf("MergedStore returned %d memories but FileStore returned %d — placement changed the semantics",
			len(viaMerged.Memories), len(viaFile.Memories))
	}
}

// An unreadable project store must not cost the user their global profile.
func TestMergedStore_UnreadableStoreDegradesToASkipNotice(t *testing.T) {
	globalDir := t.TempDir()
	writeMemFile(t, globalDir, "keep.md", "---\nid: keep\n---\nStill here.\n")

	// A regular file where a directory is expected: ReadDir fails with
	// something other than NotExist.
	bogus := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(bogus, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := NewMergedStore(NewFileStore(globalDir), NewFileStore(bogus)).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v, want the global profile preserved instead", err)
	}
	if len(res.Memories) != 1 || res.Memories[0].ID != "keep" {
		t.Errorf("memories = %+v, want the global profile intact", res.Memories)
	}
	if len(res.Skipped) != 1 {
		t.Errorf("Skipped = %+v, want the failure reported rather than swallowed", res.Skipped)
	}
}

// "both stores" has to mean both — the original test only wrote to the project
// side, so returning just the project revision would have passed.
func TestMergedStore_RevisionTracksTheGlobalStoreToo(t *testing.T) {
	global, project := NewFileStore(t.TempDir()), NewFileStore(t.TempDir())
	ms := NewMergedStore(global, project)

	before := ms.Revision(context.Background())
	if err := global.Save(context.Background(), Memory{ID: "g", Text: "G."}); err != nil {
		t.Fatal(err)
	}
	if after := ms.Revision(context.Background()); after == before {
		t.Error("Revision unchanged after a global-store write")
	}
}
