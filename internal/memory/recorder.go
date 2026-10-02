package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Recorder saves new memories. It exists so the tool layer never names an
// organization: a caller supplies text, tags, and at most a request to limit
// the memory to wherever it is being saved — never which organization that is.
// A caller that could name one could name the wrong one, and the failure mode
// is one client's conventions reaching another's work.
type Recorder struct {
	store Store
	mc    Context
	now   func() time.Time
}

func NewRecorder(store Store, mc Context) *Recorder {
	return &Recorder{store: store, mc: mc, now: time.Now}
}

// Record saves a memory that applies everywhere.
func (r *Recorder) Record(ctx context.Context, text string, tags []string) error {
	return r.RecordScoped(ctx, text, tags, false)
}

// RecordScoped saves a memory, optionally limiting it to the organization
// detected for the current directory.
//
// thisOrgOnly is a boolean rather than an org name on purpose. A caller that
// could name an organization could name the wrong one, and the failure mode is
// one client's conventions reaching another's work. Choosing between "here"
// and "everywhere" cannot reference an organization other than the detected
// one, so it carries none of that risk.
//
// Universal is the default because most preferences are: how someone wants
// commits written or tests structured does not usually stop being true when
// they change repositories. Scoping everything to wherever it happened to be
// saved made genuinely universal preferences silently stop applying.
func (r *Recorder) RecordScoped(ctx context.Context, text string, tags []string, thisOrgOnly bool) error {
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

	org := ""
	if thisOrgOnly {
		org = normalizeTag(r.mc.Org)
	}

	return r.store.Save(ctx, Memory{
		ID:   id,
		Text: text,
		Org:  org,
		// Only tags DetectContext can actually produce survive. Tags narrow by
		// intersection with the detected context, so an unrecognized one —
		// "commits", "testing", anything topical the model invents — matches
		// nothing anywhere and turns an approved memory into a stored, inert
		// file. Dropping it makes the memory broader than asked instead of
		// useless, which is at least observable.
		Tags:    knownTagsOnly(tags),
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

// knownTagsOnly keeps the tags DetectContext can produce and discards the rest.
func knownTagsOnly(tags []string) []string {
	var kept []string
	for _, t := range normalizeTags(tags) {
		if isKnownTag(t) {
			kept = append(kept, t)
		}
	}
	return kept
}
