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
type Store interface {
	Load(ctx context.Context) (LoadResult, error)
	Save(ctx context.Context, m Memory) error
	Revision(ctx context.Context) string
}

// FileStore is one directory of markdown memories.
type FileStore struct {
	dir string
	gen atomic.Uint64
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
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

// frontmatterEnd locates the closing fence: the first line that is exactly
// "---". Accepting any line merely starting with "---" would cut the block at
// a four-dash fence typo or at a dashed line inside the YAML, yielding a body
// with a stray dash rather than an error the user can act on. Returns where
// the YAML ends and where the body begins, or -1 if the fence never closes.
func frontmatterEnd(rest string) (yamlEnd, bodyStart int) {
	for i := 0; ; {
		j := strings.Index(rest[i:], "\n---")
		if j < 0 {
			return -1, -1
		}
		end := i + j
		after := end + len("\n---")
		if after == len(rest) {
			return end, after
		}
		if rest[after] == '\n' {
			return end, after + 1
		}
		i = end + 1
	}
}

// parseMemory splits a memory file into its YAML frontmatter and prose body.
func parseMemory(filename, raw string) (Memory, error) {
	body := raw
	var fm frontmatter

	if strings.HasPrefix(raw, "---\n") {
		rest := raw[len("---\n"):]
		end, bodyStart := frontmatterEnd(rest)
		if end < 0 {
			return Memory{}, fmt.Errorf("frontmatter is never closed")
		}
		if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
			return Memory{}, fmt.Errorf("invalid frontmatter: %w", err)
		}
		body = rest[bodyStart:]
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

	return Memory{ID: id, Text: text, Org: fm.Org, Tags: fm.Tags, Created: created}, nil
}

// validIDPattern keeps an id usable as a filename inside the memory directory.
var validIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func (s *FileStore) Save(ctx context.Context, m Memory) error {
	// The id becomes a path, and ids can originate from model output, so this
	// is a containment check rather than a formatting preference.
	if !validIDPattern.MatchString(m.ID) {
		return fmt.Errorf("invalid memory id %q: want lowercase words joined by hyphens", m.ID)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("creating memory dir %s: %w", s.dir, err)
	}

	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\n", m.ID)
	if !m.Created.IsZero() {
		fmt.Fprintf(&b, "created: %s\n", m.Created.Format("2006-01-02"))
	}
	if m.Org != "" {
		fmt.Fprintf(&b, "org: %s\n", m.Org)
	}
	if len(m.Tags) > 0 {
		fmt.Fprintf(&b, "tags: [%s]\n", strings.Join(m.Tags, ", "))
	}
	b.WriteString("---\n")
	b.WriteString(strings.TrimSpace(m.Text))
	b.WriteString("\n")

	path := filepath.Join(s.dir, m.ID+".md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	s.gen.Add(1)
	return nil
}

// Revision digests the name, size and mtime of every memory file, plus an
// in-process write counter. It deliberately does not use the directory's own
// mtime, which moves only when an entry is added or removed: rewriting a file
// in place leaves it untouched, so a hand-edited memory would never be
// reloaded. The counter covers this process's own saves without depending on
// filesystem timestamp granularity, which can be as coarse as a second.
//
// This runs before every model call, so it stays to one read of the directory
// and a fixed-size digest rather than an unbounded string.
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
