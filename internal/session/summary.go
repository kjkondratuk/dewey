package session

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

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

		// A launch that never got a prompt records a header and nothing else.
		// Resuming it gives empty history, which is just a fresh session, so
		// listing it is noise in the one view that has to stay scannable.
		if len(messages) == 0 {
			continue
		}

		sum := Summary{Header: header, MessageCount: len(messages)}
		sum.FirstUserMessage = pickLabel(messages)
		sums = append(sums, sum)
	}

	sort.SliceStable(sums, func(i, j int) bool {
		return sums[i].StartedAt.After(sums[j].StartedAt)
	})
	return sums, nil
}

// minLabelRunes is the length at which a user message is taken to be a
// request rather than a greeting. Chosen from real transcripts: "hello",
// "hello?", "hellos" and "continue" all fall below it, while "review the code
// in this project" and "run the tests" clear it.
const minLabelRunes = 12

// pickLabel chooses the user message that best identifies a session.
//
// Not simply the first one. Users open with "hello" and then say what they
// want, so labelling an eighteen-message code review "hello" is accurate and
// useless -- which was the measured behavior before this existed. The first
// message long enough to be a request wins; failing that the opening message
// is used anyway, because a poor label beats a blank one.
//
// Length is a proxy for substance rather than a rule about it. A genuinely
// short request is only mislabelled when a longer message precedes it, and
// then the longer one is the better label regardless.
func pickLabel(messages []*schema.Message) string {
	first := ""
	for _, m := range messages {
		if m.Role != schema.User {
			continue
		}
		text := collapseWhitespace(m.Content)
		if text == "" {
			continue
		}
		if first == "" {
			first = text
		}
		if utf8.RuneCountInString(text) >= minLabelRunes {
			return text
		}
	}
	return first
}

// collapseWhitespace folds any run of whitespace (including newlines) into a
// single space and trims the ends, so a multi-line prompt renders on one
// line. Truncation is deliberately not done here -- that's the renderer's
// job, and depends on terminal width.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
