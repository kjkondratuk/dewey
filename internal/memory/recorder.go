package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Recorder saves new memories. It exists so the tool layer never chooses the
// org: the model supplies only the text and topical tags, and the org comes
// from the detected context. A model able to set org freely could also set it
// wrongly, and the failure mode is one organization's conventions leaking into
// another's work.
type Recorder struct {
	store Store
	mc    Context
	now   func() time.Time
}

func NewRecorder(store Store, mc Context) *Recorder {
	return &Recorder{store: store, mc: mc, now: time.Now}
}

func (r *Recorder) Record(ctx context.Context, text string, tags []string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		// FileStore.Save rejects this too; saying so here names the actual
		// problem instead of complaining about an id derived from nothing.
		return fmt.Errorf("a memory needs some text")
	}
	id, err := r.availableID(ctx, slugify(text), text)
	if err != nil {
		return err
	}

	return r.store.Save(ctx, Memory{
		ID:   id,
		Text: text,
		// Normalized at both ends, like everything compared against a detected
		// context: tags come straight from the model, so a supplied "Go" would
		// otherwise be saved as a memory that can never match the detected
		// "go" — stored successfully and permanently inert.
		Org:     normalizeTag(r.mc.Org),
		Tags:    normalizeTags(tags),
		Created: r.now(),
	})
}

// availableID keeps a new memory from overwriting an existing one. The slug is
// built from the opening words of the text, and preference statements share
// those constantly ("Always run…", "Prefers that…", "Never commit…"), so
// collisions are routine rather than exotic — and because a save lands via
// rename, a collision silently destroyed a memory the user had already
// approved.
//
// Re-saving identical text keeps the same id, so correcting a memory in place
// stays idempotent instead of accumulating near-duplicates.
func (r *Recorder) availableID(ctx context.Context, base, text string) (string, error) {
	res, err := r.store.Load(ctx)
	if err != nil {
		return "", fmt.Errorf("checking for an existing memory: %w", err)
	}

	taken := make(map[string]string, len(res.Memories))
	for _, m := range res.Memories {
		taken[m.ID] = m.Text
	}

	candidate := base
	for n := 2; ; n++ {
		existing, clash := taken[candidate]
		if !clash || existing == text {
			return candidate, nil
		}
		suffix := fmt.Sprintf("-%d", n)
		trimmed := base
		if len(trimmed)+len(suffix) > maxSlugLength {
			trimmed = strings.Trim(trimmed[:maxSlugLength-len(suffix)], "-")
		}
		candidate = trimmed + suffix
	}
}

const (
	// maxSlugWords keeps the id recognizable without turning a whole sentence
	// into a filename; maxSlugLength bounds the pathological single-word case.
	maxSlugWords  = 6
	maxSlugLength = 60
)

// slugify builds a filename-safe id from the first words of the memory. The
// result has to satisfy validIDPattern or FileStore.Save refuses the memory
// after the user has already approved saving it.
func slugify(text string) string {
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}

	for _, r := range strings.ToLower(text) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			cur.WriteRune(r)
			continue
		}
		// Every other rune is a word separator rather than something to drop:
		// discarding the punctuation inside "table-driven" or "snake_case"
		// would fuse the parts into one unreadable run.
		flush()
		if len(words) == maxSlugWords {
			break
		}
	}
	flush()

	if len(words) == 0 {
		// Prose in a script with no ASCII alphanumerics leaves nothing usable.
		// A fixed stem is still a valid id; it just means the next such memory
		// overwrites this one, which beats failing the save outright.
		return "memory"
	}
	if len(words) > maxSlugWords {
		words = words[:maxSlugWords]
	}
	return truncateSlug(strings.Join(words, "-"))
}

// truncateSlug cuts an over-long slug at a word boundary where there is one
// and mid-word otherwise. Either way the result must not end in a hyphen:
// validIDPattern rejects that, and the memory would be lost.
func truncateSlug(slug string) string {
	if len(slug) <= maxSlugLength {
		return slug
	}
	cut := slug[:maxSlugLength]
	if i := strings.LastIndex(cut, "-"); i > 0 {
		return cut[:i]
	}
	return cut
}
