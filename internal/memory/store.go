package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// parseMemory splits a memory file into its YAML frontmatter and prose body.
func parseMemory(filename, raw string) (Memory, error) {
	body := raw
	var fm frontmatter

	if strings.HasPrefix(raw, "---\n") {
		rest := raw[len("---\n"):]
		end := strings.Index(rest, "\n---")
		if end < 0 {
			return Memory{}, fmt.Errorf("frontmatter is never closed")
		}
		if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
			return Memory{}, fmt.Errorf("invalid frontmatter: %w", err)
		}
		body = strings.TrimPrefix(rest[end+len("\n---"):], "\n")
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

// Revision combines an in-process write counter with the directory's mtime:
// the counter catches this process's own saves immediately, and the mtime
// catches a file edited by hand while a session is running.
func (s *FileStore) Revision(ctx context.Context) string {
	var mtime int64
	if info, err := os.Stat(s.dir); err == nil {
		mtime = info.ModTime().UnixNano()
	}
	return fmt.Sprintf("%d:%d", s.gen.Load(), mtime)
}
