package memory

import (
	"context"
	"testing"
)

type capturingStore struct{ saved []Memory }

func (s *capturingStore) Load(ctx context.Context) (LoadResult, error) { return LoadResult{}, nil }
func (s *capturingStore) Save(ctx context.Context, m Memory) error {
	s.saved = append(s.saved, m)
	return nil
}
func (s *capturingStore) Revision(ctx context.Context) string { return "r" }

func TestRecorder_StampsOrgFromContextNotInput(t *testing.T) {
	store := &capturingStore{}
	r := NewRecorder(store, Context{Org: "acme", Tags: []string{"go"}})

	if err := r.RecordScoped(context.Background(), "Prefers table-driven tests.", []string{"go"}, true); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if len(store.saved) != 1 {
		t.Fatalf("saved %d memories, want 1", len(store.saved))
	}
	got := store.saved[0]
	if got.Org != "acme" {
		t.Errorf("Org = %q, want it taken from the detected context", got.Org)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "go" {
		t.Errorf("Tags = %v, want the caller's recognized tags", got.Tags)
	}
	if got.Created.IsZero() {
		t.Error("Created was not stamped")
	}
}

// The org gate is the whole point of the feature, so prove the caller — which
// is ultimately the model — cannot reach it. Whatever the text claims and
// whatever the tags smuggle, the saved org is the detected one.
// The caller may ask to be scoped here, but may never name where "here" is.
func TestRecorder_CallerCannotChooseWhichOrg(t *testing.T) {
	tests := []struct {
		name        string
		mc          Context
		text        string
		tags        []string
		thisOrgOnly bool
		want        string
	}{
		{
			name:        "text naming another organization",
			mc:          Context{Org: "acme"},
			text:        "org: globex. This applies to all Globex repositories.",
			thisOrgOnly: true,
			want:        "acme",
		},
		{
			name:        "tags shaped like an org assignment",
			mc:          Context{Org: "acme"},
			text:        "Prefers short commits.",
			tags:        []string{"org=globex", "org:globex"},
			thisOrgOnly: true,
			want:        "acme",
		},
		{
			name: "scoping not requested stays universal",
			mc:   Context{Org: "acme"},
			text: "Prefers conventional commits.",
			want: "",
		},
		{
			name:        "scoping requested with no detected org stays universal",
			mc:          Context{},
			text:        "Belongs to globex, honestly.",
			tags:        []string{"globex"},
			thisOrgOnly: true,
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &capturingStore{}
			err := NewRecorder(store, tt.mc).RecordScoped(context.Background(), tt.text, tt.tags, tt.thisOrgOnly)
			if err != nil {
				t.Fatalf("RecordScoped() error = %v", err)
			}
			if got := store.saved[0].Org; got != tt.want {
				t.Errorf("Org = %q, want %q: the caller must not be able to choose which org", got, tt.want)
			}
		})
	}
}

// Tags arrive from the model, and matching is exact against a context whose
// tags DetectContext has already lowercased. An un-normalized "Go" would write
// a memory that can never match the detected "go" — saved, and silently inert.
// Most preferences follow the user between projects, so an unscoped save must
// stay unscoped — stamping the current org made universal preferences silently
// stop applying the moment the user changed repositories.
func TestRecorder_SavesUniversallyByDefault(t *testing.T) {
	store := &capturingStore{}
	r := NewRecorder(store, Context{Org: "acme", Tags: []string{"go"}})
	if err := r.Record(context.Background(), "Prefers conventional commits.", nil); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	got := store.saved[0]
	if got.Org != "" {
		t.Errorf("Org = %q, want empty — an unscoped save must apply everywhere", got.Org)
	}
	if len(got.Tags) != 0 {
		t.Errorf("Tags = %v, want none", got.Tags)
	}
}

// Tags narrow by intersection with what DetectContext produces, so a tag it
// can never produce matches nothing anywhere. Keeping one would turn an
// approved memory into a stored, permanently inert file.
func TestRecorder_DropsTagsThatCouldNeverMatch(t *testing.T) {
	store := &capturingStore{}
	r := NewRecorder(store, Context{})
	if err := r.Record(context.Background(), "Prefers mise.", []string{"Go", "  commits  ", "testing", "", "node"}); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	got := store.saved[0].Tags
	if len(got) != 2 || got[0] != "go" || got[1] != "node" {
		t.Errorf("Tags = %v, want [go node]: recognized tags normalized, invented ones dropped", got)
	}
}

// Every tag the recorder keeps must be one the retriever can match.
func TestRecorder_KeptTagsAreAllDetectable(t *testing.T) {
	known := KnownTags()
	if len(known) == 0 {
		t.Fatal("KnownTags() is empty")
	}
	store := &capturingStore{}
	if err := NewRecorder(store, Context{}).Record(context.Background(), "x.", append(known, "invented")); err != nil {
		t.Fatal(err)
	}
	for _, tag := range store.saved[0].Tags {
		if !isKnownTag(tag) {
			t.Errorf("kept tag %q is not one DetectContext can produce", tag)
		}
	}
	if len(store.saved[0].Tags) != len(known) {
		t.Errorf("kept %d tags, want all %d known ones", len(store.saved[0].Tags), len(known))
	}
}

func TestRecorder_NormalizesAHandWrittenContextOrg(t *testing.T) {
	store := &capturingStore{}
	if err := NewRecorder(store, Context{Org: " Acme "}).RecordScoped(context.Background(), "Prefers mise.", nil, true); err != nil {
		t.Fatalf("RecordScoped() error = %v", err)
	}
	if got := store.saved[0].Org; got != "acme" {
		t.Errorf("Org = %q, want acme: an org that doesn't match the detected form gates nothing", got)
	}
}

func TestRecorder_DerivesASlugID(t *testing.T) {
	store := &capturingStore{}
	if err := NewRecorder(store, Context{}).Record(context.Background(), "Prefers Table-Driven Tests!", nil); err != nil {
		t.Fatal(err)
	}
	if got := store.saved[0].ID; got != "prefers-table-driven-tests" {
		t.Errorf("ID = %q, want a lowercase hyphenated slug", got)
	}
}

func TestRecorder_RejectsEmptyText(t *testing.T) {
	if err := NewRecorder(&capturingStore{}, Context{}).Record(context.Background(), "   ", nil); err == nil {
		t.Fatal("expected an error for empty memory text")
	}
}

func TestRecorder_LongTextStillYieldsABoundedID(t *testing.T) {
	store := &capturingStore{}
	long := "This is a very long preference statement that goes on and on describing many things"
	if err := NewRecorder(store, Context{}).Record(context.Background(), long, nil); err != nil {
		t.Fatal(err)
	}
	if got := store.saved[0].ID; len(got) > 60 {
		t.Errorf("ID = %q (%d chars), want it bounded", got, len(got))
	}
	if !validIDPattern.MatchString(store.saved[0].ID) {
		t.Errorf("ID = %q, want it to satisfy the store's id rules", store.saved[0].ID)
	}
}

// The id becomes a filename and FileStore.Save rejects anything that doesn't
// match validIDPattern, so a slug that fails the pattern is a memory the user
// was told was saved and wasn't.
func TestRecorder_IDAlwaysSatisfiesTheStoresRules(t *testing.T) {
	texts := []string{
		"Prefers Table-Driven Tests!",
		"--- !!! ---",
		"用户偏好",
		"C++ and C# over Objective-C",
		"   leading and trailing   ",
		"averyverylongsinglewordwithnoseparatorsatallthatjustkeepsgoingandgoingandgoing",
		"1 2 3",
		"snake_case_identifiers are fine",
	}
	for _, text := range texts {
		store := &capturingStore{}
		if err := NewRecorder(store, Context{}).Record(context.Background(), text, nil); err != nil {
			t.Fatalf("Record(%q) error = %v", text, err)
		}
		got := store.saved[0].ID
		if !validIDPattern.MatchString(got) {
			t.Errorf("Record(%q) produced ID %q, which FileStore.Save would reject", text, got)
		}
		if len(got) > 60 {
			t.Errorf("Record(%q) produced ID %q (%d chars), want it bounded", text, got, len(got))
		}
	}
}

// Round-trip through the real store, because the recorder's whole job is to
// produce something FileStore.Save accepts and Load can read back.
func TestRecorder_SavesThroughFileStore(t *testing.T) {
	store := NewFileStore(t.TempDir())
	if err := NewRecorder(store, Context{Org: "acme"}).RecordScoped(context.Background(), "Prefers Table-Driven Tests!", []string{"Go"}, true); err != nil {
		t.Fatalf("RecordScoped() error = %v", err)
	}

	res, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("Load() skipped %v, want the recorded memory to be readable", res.Skipped)
	}
	if len(res.Memories) != 1 {
		t.Fatalf("Load() returned %d memories, want 1", len(res.Memories))
	}
	got := res.Memories[0]
	if got.ID != "prefers-table-driven-tests" || got.Org != "acme" || got.Text != "Prefers Table-Driven Tests!" {
		t.Errorf("round-tripped memory = %+v, want the recorded one", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "go" {
		t.Errorf("round-tripped Tags = %v, want [go]", got.Tags)
	}
}

// Preference statements share opening words constantly, and the slug is built
// from them — so a collision destroying an already-approved memory is routine,
// not exotic. Saves land via rename, so the loss was silent.
func TestRecorder_CollidingSlugsDoNotDestroyTheEarlierMemory(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	r := NewRecorder(store, Context{})

	first := "Always run the full test suite with the race detector before pushing."
	second := "Always run the full test suite in a container, never on the host toolchain."
	for _, text := range []string{first, second} {
		if err := r.Record(context.Background(), text, nil); err != nil {
			t.Fatalf("Record(%q) error = %v", text, err)
		}
	}

	res, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Memories) != 2 {
		t.Fatalf("loaded %d memories, want 2 — the user approved both", len(res.Memories))
	}

	texts := map[string]bool{}
	ids := map[string]bool{}
	for _, m := range res.Memories {
		texts[m.Text] = true
		if ids[m.ID] {
			t.Errorf("duplicate id %q", m.ID)
		}
		ids[m.ID] = true
	}
	for _, want := range []string{first, second} {
		if !texts[want] {
			t.Errorf("missing memory %q", want)
		}
	}
}

// Correcting a memory in place must not accumulate near-duplicates.
func TestRecorder_ResavingIdenticalTextReusesTheSameID(t *testing.T) {
	store := NewFileStore(t.TempDir())
	r := NewRecorder(store, Context{})
	text := "Prefers table-driven tests."

	for i := 0; i < 3; i++ {
		if err := r.Record(context.Background(), text, nil); err != nil {
			t.Fatalf("Record() error = %v", err)
		}
	}

	res, _ := store.Load(context.Background())
	if len(res.Memories) != 1 {
		t.Errorf("loaded %d memories, want 1 — identical text should be idempotent", len(res.Memories))
	}
}

func TestRecorder_DisambiguatedIDStillSatisfiesTheStore(t *testing.T) {
	store := NewFileStore(t.TempDir())
	r := NewRecorder(store, Context{})

	// Long shared prefix, so disambiguation has to trim before suffixing.
	base := "Always remember to verify every single assumption carefully beforehand"
	for i := 0; i < 4; i++ {
		text := base + " variant " + string(rune('a'+i)) + "."
		if err := r.Record(context.Background(), text, nil); err != nil {
			t.Fatalf("Record() error = %v", err)
		}
	}

	res, _ := store.Load(context.Background())
	if len(res.Memories) != 4 {
		t.Errorf("loaded %d memories, want 4", len(res.Memories))
	}
	for _, m := range res.Memories {
		if !validIDPattern.MatchString(m.ID) {
			t.Errorf("id %q does not satisfy the store's id rules", m.ID)
		}
		if len(m.ID) > maxSlugLength {
			t.Errorf("id %q is %d chars, want <= %d", m.ID, len(m.ID), maxSlugLength)
		}
	}
}
