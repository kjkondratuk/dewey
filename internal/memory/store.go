package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
