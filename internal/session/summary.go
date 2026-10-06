package session

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// Summary is what identifying a session actually requires: the header, plus
// the two things that can only be learned by reading the body.
type Summary struct {
	Header
	// FirstUserMessage is what the user opened with, which is the most
	// reliable label a session has. Empty when the session recorded no user
	// message at all.
	FirstUserMessage string
	// MessageCount counts recorded messages, excluding events and the header.
	MessageCount int
}

// Summaries returns the recorded sessions under dir, newest first.
//
// Unlike a header-only read this parses each file, which is what lets the
// listing say anything about the conversation. A transcript is tens of
// kilobytes, so one pass per file costs microseconds; if that ever stops
// being true the answer is a recorded title, not a lazier read.
func Summaries(dir string) ([]Summary, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var sums []Summary
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}

		messages, header, err := Load(filepath.Join(dir, e.Name()))
		if err != nil {
			// Unreadable file: skip it, same as the old header-only List did.
			continue
		}

		sum := Summary{Header: header, MessageCount: len(messages)}
		for _, m := range messages {
			if m.Role == schema.User {
				sum.FirstUserMessage = collapseWhitespace(m.Content)
				break
			}
		}
		sums = append(sums, sum)
	}

	sort.SliceStable(sums, func(i, j int) bool {
		return sums[i].StartedAt.After(sums[j].StartedAt)
	})
	return sums, nil
}

// collapseWhitespace folds any run of whitespace (including newlines) into a
// single space and trims the ends, so a multi-line prompt renders on one
// line. Truncation is deliberately not done here -- that's the renderer's
// job, and depends on terminal width.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
