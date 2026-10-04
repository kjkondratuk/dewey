// Package files tracks which files a session has read, so tools that modify
// files can refuse to work from contents they never looked at.
package files

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Tracker records the files read during one session and the mtime each had
// when it was read.
//
// This exists because Edit and Write are only safe if the model is working
// from the file's current contents. Without it, Edit degrades to blind string
// replacement: a stale old_string either fails to match or, worse, matches
// something the model did not intend, and the failure looks like a model
// mistake rather than a missing precondition.
type Tracker struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func NewTracker() *Tracker {
	return &Tracker{seen: make(map[string]time.Time)}
}

// key normalizes a path so one file read and written through different
// spellings resolves to a single entry.
func key(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return abs
}

// MarkRead records that path was read when it had the given mtime.
func (t *Tracker) MarkRead(path string, mtime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen[key(path)] = mtime
}

// ReadAt returns the mtime path had when it was read this session.
func (t *Tracker) ReadAt(path string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	mtime, ok := t.seen[key(path)]
	return mtime, ok
}

// CheckWritable reports why path must not be modified yet, or nil when it may
// be. A file that does not exist is always writable — requiring a read before
// creating a new file would block the common case.
func (t *Tracker) CheckWritable(path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	readAt, ok := t.ReadAt(path)
	if !ok {
		return fmt.Errorf("%s exists but has not been read this session; read it first so the change is based on its current contents", path)
	}
	if !info.ModTime().Equal(readAt) {
		return fmt.Errorf("%s changed on disk since it was read; read it again before modifying it", path)
	}
	return nil
}
