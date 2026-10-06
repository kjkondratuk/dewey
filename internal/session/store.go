// Package session persists a dewey conversation to a JSONL transcript and
// loads it back for resume. It exists as debugging instrumentation for a live
// bug (see internal/agent/openai.go's history-handling comments): the
// transcript has to show, without guesswork, the exact assistant text per
// turn, how long any reasoning_content was, how many messages were sent on
// each request, and every compaction / notice. Recording a session must never
// be able to break one -- every write failure is swallowed and surfaced only
// through Err().
package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

// ToolCall is the recorded shape of a schema.ToolCall: just enough to
// reconstruct one on Load.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
}

// Header describes one session: the environment it ran in and the models it
// used. It is written once, as the first record in the file.
type Header struct {
	ID                string `json:"id"`
	CWD               string `json:"cwd"`
	ProjectRoot       string `json:"project_root"`
	IntermediateModel string `json:"intermediate_model"`
	SimpleModel       string `json:"simple_model"`
	ContextWindow     int    `json:"context_window"`
	// ResumedFrom is the id of the session this one continues, set only when
	// the run was started with --resume. A fresh session leaves it empty.
	ResumedFrom string `json:"resumed_from,omitempty"`
	// StartedAt is set automatically by NewStore when zero. It exists so a
	// cheap header-only read (see List) can report when a session began
	// without having to stat the file or scan its body.
	StartedAt time.Time `json:"started_at,omitempty"`
}

// Record is one line of a session transcript.
type Record struct {
	Seq  int       `json:"seq"`
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // "header" | "message" | "event"

	// Message fields, set when Kind == "message".
	Role         string     `json:"role,omitempty"`
	Content      string     `json:"content,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID   string     `json:"tool_call_id,omitempty"`
	ToolName     string     `json:"tool_name,omitempty"`
	ReasoningLen int        `json:"reasoning_len,omitempty"`

	// Event fields, set when Kind == "event".
	Event  string `json:"event,omitempty"`
	Detail string `json:"detail,omitempty"`

	// Header fields, set when Kind == "header".
	Header *Header `json:"header,omitempty"`
}

// Store appends JSONL records to one session file. It is safe for concurrent
// use: Append and Event may be called from multiple goroutines.
type Store struct {
	mu   sync.Mutex
	f    *os.File
	id   string
	path string
	seq  int
	err  error
}

// NewStore creates a session file under dir and writes the header record
// first. If h.ID is empty a UUID is generated. dir is created with 0o700
// (conversations are private) and the file with 0o600. The returned Store
// owns the file until Close.
func NewStore(dir string, h Header) (*Store, error) {
	if h.ID == "" {
		h.ID = uuid.NewString()
	}
	if h.StartedAt.IsZero() {
		h.StartedAt = time.Now()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating session directory: %w", err)
	}

	path := filepath.Join(dir, h.ID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("creating session file: %w", err)
	}

	s := &Store{f: f, id: h.ID, path: path}
	header := h
	if err := s.write(Record{Kind: "header", Header: &header}); err != nil {
		f.Close()
		return nil, fmt.Errorf("writing session header: %w", err)
	}
	return s, nil
}

// ID returns the session's id.
func (s *Store) ID() string { return s.id }

// Path returns the path to the session's transcript file.
func (s *Store) Path() string { return s.path }

// Err returns the first write error encountered, or nil if every write has
// succeeded so far. Append and Event never return errors directly -- a
// recording failure must never break the session it is recording -- so this
// is how a caller surfaces a notice about it.
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Append records msg as a "message" record. ReasoningLen is derived from
// len(msg.ReasoningContent) -- the reasoning text itself is never written,
// only its length, so compounding is visible as a number without the
// (potentially large, potentially sensitive) content being duplicated into
// the transcript. A write failure is swallowed; see Err.
func (s *Store) Append(msg *schema.Message) {
	if msg == nil {
		return
	}

	toolName := msg.ToolName
	if toolName == "" {
		toolName = msg.Name
	}

	rec := Record{
		Kind:         "message",
		Role:         string(msg.Role),
		Content:      msg.Content,
		ToolCallID:   msg.ToolCallID,
		ToolName:     toolName,
		ReasoningLen: len(msg.ReasoningContent),
	}
	for _, tc := range msg.ToolCalls {
		rec.ToolCalls = append(rec.ToolCalls, ToolCall{
			ID:   tc.ID,
			Name: tc.Function.Name,
			Args: tc.Function.Arguments,
		})
	}

	_ = s.write(rec)
}

// Event records an out-of-band occurrence: a compaction, a forced
// compaction, a notice shown to the user, a cancellation, or an error. A
// write failure is swallowed; see Err.
func (s *Store) Event(event, detail string) {
	_ = s.write(Record{Kind: "event", Event: event, Detail: detail})
}

// Close closes the underlying file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

// write assigns Seq and At, marshals rec, and flushes it to disk immediately
// -- no buffering is held across calls, so a session that crashes mid-loop is
// exactly the one that still reads back cleanly up to its last complete line.
// Any error is recorded (the first one only) rather than returned, so a
// caller that ignores it -- which Append and Event both do on purpose -- can
// never be surprised by a panic.
func (s *Store) write(rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	rec.Seq = s.seq
	rec.At = time.Now()

	data, err := json.Marshal(rec)
	if err != nil {
		s.setErrLocked(err)
		return err
	}
	data = append(data, '\n')

	if _, err := s.f.Write(data); err != nil {
		s.setErrLocked(err)
		return err
	}
	if err := s.f.Sync(); err != nil {
		s.setErrLocked(err)
		return err
	}
	return nil
}

func (s *Store) setErrLocked(err error) {
	if s.err == nil {
		s.err = err
	}
}

// Load reads a session transcript back into the message history it recorded,
// plus its header. Event records are skipped. Unparseable lines are skipped
// rather than failing the load -- a truncated final line is expected when a
// session was killed, which is the common case for the sessions worth
// resuming.
//
// Reasoning content is NOT restored -- only its length was ever recorded --
// which is a deliberate, documented limitation: a resumed session is not
// byte-identical to the original request stream.
func Load(path string) ([]*schema.Message, Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, Header{}, err
	}
	defer f.Close()

	var header Header
	var messages []*schema.Message

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}

		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			// A truncated final line, or any other corruption: skip it. The
			// transcript up to this point is still worth loading.
			continue
		}

		switch rec.Kind {
		case "header":
			if rec.Header != nil {
				header = *rec.Header
			}
		case "message":
			messages = append(messages, recordToMessage(rec))
		case "event":
			// Events describe what happened, not what was said; resuming a
			// session continues the conversation, not the event log.
		}
	}

	return messages, header, nil
}

func recordToMessage(rec Record) *schema.Message {
	msg := &schema.Message{
		Role:       schema.RoleType(rec.Role),
		Content:    rec.Content,
		ToolCallID: rec.ToolCallID,
		// Both ToolName and Name are restored from the single recorded value:
		// toolResult() in internal/agent/openai.go always sets them to the
		// same string (eino's OpenAI adapter only serializes Message.Name,
		// while WithToolName only populates Message.ToolName), so one
		// recorded field is sufficient to restore both faithfully.
		ToolName: rec.ToolName,
		Name:     rec.ToolName,
	}
	for _, tc := range rec.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{
			ID:   tc.ID,
			Type: "function",
			Function: schema.FunctionCall{
				Name:      tc.Name,
				Arguments: tc.Args,
			},
		})
	}
	return msg
}

// List returns the sessions under dir, newest first, reading only each
// file's header record -- the first line of each file -- so listing is cheap
// even for a long transcript.
func List(dir string) ([]Header, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	type dated struct {
		h  Header
		at time.Time
	}
	var items []dated
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		rec, ok := readHeaderRecord(filepath.Join(dir, e.Name()))
		if !ok {
			continue
		}
		items = append(items, dated{h: *rec.Header, at: rec.At})
	}

	sort.SliceStable(items, func(i, j int) bool {
		return items[i].at.After(items[j].at)
	})

	headers := make([]Header, len(items))
	for i, it := range items {
		headers[i] = it.h
	}
	return headers, nil
}

// readHeaderRecord reads only the first line of path and reports whether it
// is a valid header record.
func readHeaderRecord(path string) (Record, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Record{}, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	if !sc.Scan() {
		return Record{}, false
	}

	var rec Record
	if err := json.Unmarshal(bytes.TrimSpace(sc.Bytes()), &rec); err != nil {
		return Record{}, false
	}
	if rec.Kind != "header" || rec.Header == nil {
		return Record{}, false
	}
	return rec, true
}

// Resolve finds a session file by full or partial id under dir, so a user can
// type the first few characters of a UUID. An exact filename match always
// wins; otherwise exactly one prefix match is required.
func Resolve(dir, id string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}

	var matches []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".jsonl")
		if name == id {
			return filepath.Join(dir, e.Name()), nil
		}
		if strings.HasPrefix(name, id) {
			matches = append(matches, filepath.Join(dir, e.Name()))
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no session found matching %q", id)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous session id %q matches %d sessions", id, len(matches))
	}
}
