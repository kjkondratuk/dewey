package memory

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// SkipNotice records a file that could not be loaded. Surfacing these matters:
// a silently ignored memory is a preference the user believes is in effect.
type SkipNotice struct {
	File   string
	Reason string
}

// LoadResult is everything one Load produced, including what it couldn't read.
type LoadResult struct {
	Memories []Memory
	Skipped  []SkipNotice
}

// Store reads and writes memories. Revision is a cheap change token: when it
// differs from a previously observed value, callers must reload.
//
// All three methods are safe to call concurrently. Note that -race cannot
// prove this: the hazard is in the filesystem, not in Go memory, so writers
// are serialized explicitly and writes land via a rename.
type Store interface {
	Load(ctx context.Context) (LoadResult, error)
	Save(ctx context.Context, m Memory) error
	Revision(ctx context.Context) string
}

// FileStore is one directory of markdown memories.
type FileStore struct {
	dir string
	gen atomic.Uint64
	// writeMu serializes writers. The atomic counter alone would not: two
	// concurrent saves of the same id both target the same path.
	writeMu sync.Mutex
}

func NewFileStore(dir string) *FileStore {
	return &FileStore{dir: dir}
}

// Dir reports the directory this store reads and writes.
func (s *FileStore) Dir() string { return s.dir }

type frontmatter struct {
	ID      string   `yaml:"id"`
	Org     string   `yaml:"org"`
	Tags    []string `yaml:"tags"`
	Created string   `yaml:"created"`
}

func (s *FileStore) Load(ctx context.Context) (LoadResult, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			// A user who has never saved a memory is not an error.
			return LoadResult{}, nil
		}
		return LoadResult{}, fmt.Errorf("reading memory dir %s: %w", s.dir, err)
	}

	var res LoadResult
	for _, e := range entries {
		if e.IsDir() || !isMarkdownName(e.Name()) {
			continue
		}
		// Refuse anything that is not a regular file. A project memory
		// directory is meant to be committed, so a cloned repo can contain a
		// symlink -- and these files are read straight into the system
		// message, the highest-authority slot there is. A link pointing at
		// ~/.ssh/id_ed25519 would put key material in the model's context.
		if !e.Type().IsRegular() {
			res.Skipped = append(res.Skipped, SkipNotice{
				File:   e.Name(),
				Reason: "not a regular file; refusing to read it into the system prompt",
			})
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			res.Skipped = append(res.Skipped, SkipNotice{File: e.Name(), Reason: err.Error()})
			continue
		}
		m, err := parseMemory(e.Name(), string(raw))
		if err != nil {
			res.Skipped = append(res.Skipped, SkipNotice{File: e.Name(), Reason: err.Error()})
			continue
		}
		res.Memories = append(res.Memories, m)
	}
	return res, nil
}

// isMarkdownName matches the .md extension case-insensitively. A file named
// Pref.MD was previously ignored in silence -- exactly the "a memory the user
// believes is in effect" failure SkipNotice exists to prevent -- and on a
// case-insensitive filesystem it is indistinguishable from pref.md anyway.
func isMarkdownName(name string) bool {
	return len(name) > 3 && strings.EqualFold(name[len(name)-3:], ".md")
}

// isFenceLine reports whether a line is a frontmatter delimiter: exactly
// "---" once trailing whitespace is ignored. A line merely starting with "---"
// must not count, or a four-dash typo or a dashed line inside the YAML would
// cut the block short and yield a mangled body instead of an actionable error.
func isFenceLine(line string) bool {
	return strings.TrimRight(line, " \t\r") == "---"
}

// parseMemory splits a memory file into its YAML frontmatter and prose body.
//
// Fence recognition is deliberately lenient about encoding, because failing to
// recognize frontmatter is not a cosmetic problem: an unparsed file keeps an
// empty Org, and an empty Org means "applies to every organization". A memory
// scoped to one client would silently start influencing work for another, and
// the raw YAML would be fed to the model as prose on top of that. CRLF line
// endings, a UTF-8 BOM, or a trailing space after the fence are all ordinary
// things an editor produces, so none of them may cause that.
func parseMemory(filename, raw string) (Memory, error) {
	raw = strings.TrimPrefix(raw, "\ufeff")
	raw = strings.ReplaceAll(raw, "\r\n", "\n")

	body := raw
	var fm frontmatter

	lines := strings.Split(raw, "\n")

	// Skip blank lines ahead of the fence. A stray newline at the top of the
	// file is a trivial editor artifact, and treating it as "no frontmatter"
	// is the expensive interpretation: it would turn a scoped memory into a
	// universal one.
	open := 0
	for open < len(lines) && strings.TrimSpace(lines[open]) == "" {
		open++
	}

	if open < len(lines) && isFenceLine(lines[open]) {
		closed := false
		for i := open + 1; i < len(lines); i++ {
			if !isFenceLine(lines[i]) {
				continue
			}
			// An empty frontmatter block (fence immediately followed by fence)
			// is a legitimate way to write a universal memory, so the scan
			// starts at the line after the opening fence rather than requiring
			// at least one key.
			if err := yaml.Unmarshal([]byte(strings.Join(lines[open+1:i], "\n")), &fm); err != nil {
				return Memory{}, fmt.Errorf("invalid frontmatter: %w", err)
			}
			body = strings.Join(lines[i+1:], "\n")
			closed = true
			break
		}
		if !closed {
			return Memory{}, fmt.Errorf("frontmatter is never closed")
		}
	}

	id := fm.ID
	if id == "" {
		id = strings.TrimSuffix(filename, ".md")
	}

	text := strings.TrimSpace(body)
	if text == "" {
		return Memory{}, fmt.Errorf("memory has no body text")
	}

	var created time.Time
	if fm.Created != "" {
		// A bad date shouldn't cost the user the memory; it only affects the
		// order things are dropped in when the budget is tight.
		created, _ = time.Parse("2006-01-02", fm.Created)
	}

	return Memory{
		ID:      id,
		Text:    text,
		Org:     normalizeTag(fm.Org),
		Tags:    normalizeTags(fm.Tags),
		Created: created,
	}, nil
}

// normalizeTag lowercases and trims an org or tag. Matching is exact, and
// DetectContext yields lowercase values, so a hand-written "org: Acme" or
// "tags: [Go]" would otherwise never match anything. That fails closed — the
// memory just stops applying — which makes it quiet and confusing rather than
// dangerous, and it has to be done at both ends to work at all.
func normalizeTag(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if n := normalizeTag(t); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// validIDPattern keeps an id usable as a filename inside the memory directory.
var validIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func (s *FileStore) Save(ctx context.Context, m Memory) error {
	// The id becomes a path, and ids can originate from model output, so this
	// is a containment check rather than a formatting preference.
	if !validIDPattern.MatchString(m.ID) {
		return fmt.Errorf("invalid memory id %q: want lowercase words joined by hyphens", m.ID)
	}
	// Symmetry with the id check above: Load rejects a body-less memory, so
	// accepting one here would report success and write a file that is
	// unreadable from that moment on.
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return fmt.Errorf("refusing to save memory %q with no text", m.ID)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("creating memory dir %s: %w", s.dir, err)
	}

	// Marshal rather than Fprintf the frontmatter. Org, Tags and Text can all
	// originate from model output, and interpolating them raw produced files
	// that Save accepted and Load could then never read again — a newline in
	// Org emitted a second `id:` key, a `]` in a tag broke the flow sequence.
	// The user was told the memory was saved and it was permanently unreadable.
	head, err := yaml.Marshal(frontmatter{
		ID:      m.ID,
		Org:     m.Org,
		Tags:    m.Tags,
		Created: createdString(m.Created),
	})
	if err != nil {
		return fmt.Errorf("encoding frontmatter for %s: %w", m.ID, err)
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.Write(head)
	b.WriteString("---\n")
	b.WriteString(text)
	b.WriteString("\n")

	return s.writeAtomic(m.ID+".md", b.String())
}

func createdString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}

// writeAtomic writes via a temp file and a rename, so a concurrent Load never
// observes a half-written file. Without it, Provider revalidating before every
// model call while the remember tool is mid-write produced torn reads and a
// stream of spurious skip notices about a corpus that was in fact fine.
func (s *FileStore) writeAtomic(name, contents string) error {
	// Serialize writers: two concurrent saves of the same id would otherwise
	// race on the same destination path.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tmp, err := os.CreateTemp(s.dir, ".tmp-"+name+"-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", s.dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.WriteString(contents); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("setting mode on %s: %w", tmpName, err)
	}

	path := filepath.Join(s.dir, name)
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming into %s: %w", path, err)
	}
	s.gen.Add(1)
	return nil
}

// Revision digests the name, size and mtime of every memory file, plus an
// in-process write counter. It deliberately does not use the directory's own
// mtime, which moves only when an entry is added or removed: rewriting a file
// in place leaves it untouched, so a hand-edited memory would never be
// reloaded.
//
// The counter makes this process's own saves register without relying on
// timestamp resolution. External edits do rely on it, so an edit that changes
// neither size nor mtime is invisible — unreachable for a real editor save on
// a filesystem with nanosecond mtimes, but worth knowing.
//
// This runs before every model call. The digest is fixed-size, though the work
// is O(files): one directory read plus a stat per entry, which is what
// dominates.
func (s *FileStore) Revision(ctx context.Context) string {
	h := fnv.New64a()
	buf := binary.LittleEndian.AppendUint64(make([]byte, 0, 64), s.gen.Load())
	h.Write(buf)

	// A missing or unreadable directory is a stable state, not something to
	// report here; Load is where that surfaces.
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return strconv.FormatUint(h.Sum64(), 16)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// Vanished mid-scan; treat it as absent and let the next call settle.
			continue
		}
		buf = buf[:0]
		buf = append(buf, e.Name()...)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(info.Size()))
		buf = binary.LittleEndian.AppendUint64(buf, uint64(info.ModTime().UnixNano()))
		h.Write(buf)
	}
	return strconv.FormatUint(h.Sum64(), 16)
}
