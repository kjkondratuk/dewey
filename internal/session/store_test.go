package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

func readRawRecords(t *testing.T, path string) []Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var recs []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal line %q: %v", line, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

// 1. NewStore writes a header first, ID() is non-empty, the file exists at
// Path() with mode 0600, and the directory is 0700.
func TestNewStore_WritesHeaderAndSetsPermissions(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "sessions")

	s, err := NewStore(dir, Header{CWD: "/work", ProjectRoot: "/work", IntermediateModel: "big", SimpleModel: "small", ContextWindow: 8192})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer s.Close()

	if s.ID() == "" {
		t.Error("ID() is empty, want a generated UUID")
	}

	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat %s: %v", s.Path(), err)
	}
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("file mode = %o, want 0600", mode)
		}
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir %s: %v", dir, err)
	}
	if runtime.GOOS != "windows" {
		if mode := dirInfo.Mode().Perm(); mode != 0o700 {
			t.Errorf("dir mode = %o, want 0700", mode)
		}
	}

	recs := readRawRecords(t, s.Path())
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1 (just the header)", len(recs))
	}
	if recs[0].Kind != "header" {
		t.Errorf("Kind = %q, want header", recs[0].Kind)
	}
	if recs[0].Header == nil || recs[0].Header.ID != s.ID() {
		t.Errorf("Header = %+v, want ID %q", recs[0].Header, s.ID())
	}
	if recs[0].Header.StartedAt.IsZero() {
		t.Error("Header.StartedAt is zero, want it auto-set so a cheap listing can report when the session began")
	}
}

// 2. Round trip: a user message, an assistant message with two tool calls,
// and two tool results. Load must reconstruct all four with roles, content,
// tool call ids/names/args, and ToolCallID intact.
func TestStore_RoundTrip_MessagesWithToolCalls(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, Header{})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	s.Append(&schema.Message{Role: schema.User, Content: "what's the weather?"})

	assistant := &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{
			{ID: "call-1", Type: "function", Function: schema.FunctionCall{Name: "get_weather", Arguments: `{"city":"nyc"}`}},
			{ID: "call-2", Type: "function", Function: schema.FunctionCall{Name: "get_time", Arguments: `{"tz":"est"}`}},
		},
	}
	s.Append(assistant)

	tool1 := schema.ToolMessage("sunny", "call-1", schema.WithToolName("get_weather"))
	tool1.Name = "get_weather"
	s.Append(tool1)

	tool2 := schema.ToolMessage("3pm", "call-2", schema.WithToolName("get_time"))
	tool2.Name = "get_time"
	s.Append(tool2)

	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	msgs, _, err := Load(s.Path())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("len(msgs) = %d, want 4", len(msgs))
	}

	if msgs[0].Role != schema.User || msgs[0].Content != "what's the weather?" {
		t.Errorf("msgs[0] = %+v, want the user message", msgs[0])
	}

	if msgs[1].Role != schema.Assistant {
		t.Errorf("msgs[1].Role = %q, want assistant", msgs[1].Role)
	}
	if len(msgs[1].ToolCalls) != 2 {
		t.Fatalf("msgs[1].ToolCalls = %d, want 2", len(msgs[1].ToolCalls))
	}
	if msgs[1].ToolCalls[0].ID != "call-1" || msgs[1].ToolCalls[0].Function.Name != "get_weather" || msgs[1].ToolCalls[0].Function.Arguments != `{"city":"nyc"}` {
		t.Errorf("msgs[1].ToolCalls[0] = %+v, want call-1/get_weather", msgs[1].ToolCalls[0])
	}
	if msgs[1].ToolCalls[1].ID != "call-2" || msgs[1].ToolCalls[1].Function.Name != "get_time" {
		t.Errorf("msgs[1].ToolCalls[1] = %+v, want call-2/get_time", msgs[1].ToolCalls[1])
	}

	if msgs[2].Role != schema.Tool || msgs[2].Content != "sunny" || msgs[2].ToolCallID != "call-1" || msgs[2].ToolName != "get_weather" {
		t.Errorf("msgs[2] = %+v, want tool result for call-1", msgs[2])
	}
	if msgs[3].Role != schema.Tool || msgs[3].Content != "3pm" || msgs[3].ToolCallID != "call-2" || msgs[3].ToolName != "get_time" {
		t.Errorf("msgs[3] = %+v, want tool result for call-2", msgs[3])
	}
}

// 3. ReasoningLen is recorded from ReasoningContent, and the reasoning text
// itself never touches disk. This is the whole point of the instrument: the
// length can grow without the (potentially enormous, potentially sensitive)
// content being duplicated into every record.
func TestStore_ReasoningContentLengthOnlyNotContent(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, Header{})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	const reasoningMarker = "UNIQUE_REASONING_TEXT_MARKER_DO_NOT_PERSIST"
	msg := &schema.Message{
		Role:             schema.Assistant,
		Content:          "the answer is 42",
		ReasoningContent: reasoningMarker + strings.Repeat("x", 100),
	}
	s.Append(msg)
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(raw), reasoningMarker) {
		t.Fatal("raw file contains reasoning content; it must only record its length")
	}

	recs := readRawRecords(t, s.Path())
	var found bool
	for _, r := range recs {
		if r.Kind == "message" && r.Role == string(schema.Assistant) {
			found = true
			if r.ReasoningLen != len(msg.ReasoningContent) {
				t.Errorf("ReasoningLen = %d, want %d", r.ReasoningLen, len(msg.ReasoningContent))
			}
		}
	}
	if !found {
		t.Fatal("no assistant message record found")
	}
}

// 4. Event records are written and are skipped by Load.
func TestStore_EventsWrittenAndSkippedByLoad(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, Header{})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	s.Append(&schema.Message{Role: schema.User, Content: "hi"})
	s.Event("compacted", "replaced 10 messages")
	s.Event("notice", "context window updated")
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	recs := readRawRecords(t, s.Path())
	var eventCount int
	for _, r := range recs {
		if r.Kind == "event" {
			eventCount++
		}
	}
	if eventCount != 2 {
		t.Errorf("event records = %d, want 2", eventCount)
	}

	msgs, _, err := Load(s.Path())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("len(msgs) = %d, want 1 (events skipped)", len(msgs))
	}
}

// 5. A truncated final line (the common case for a killed session) must not
// prevent loading everything before it.
func TestLoad_TruncatedFinalLineStillLoadsCompleteRecords(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, Header{})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	s.Append(&schema.Message{Role: schema.User, Content: "one"})
	s.Append(&schema.Message{Role: schema.Assistant, Content: "two"})
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if len(data) < 5 {
		t.Fatalf("file too short to truncate: %d bytes", len(data))
	}
	truncated := data[:len(data)-5]
	if err := os.WriteFile(path, truncated, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	msgs, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want no error even with a truncated tail", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1 complete record to survive", len(msgs))
	}
	if msgs[0].Content != "one" {
		t.Errorf("msgs[0].Content = %q, want %q", msgs[0].Content, "one")
	}
}

// 6. List returns newest-first and reads only headers -- a garbage body must
// not prevent a file's header from being listed.
func TestList_NewestFirstHeadersOnly(t *testing.T) {
	dir := t.TempDir()

	s1, err := NewStore(dir, Header{CWD: "/first"})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	s1.Close()
	time.Sleep(5 * time.Millisecond)

	s2, err := NewStore(dir, Header{CWD: "/second"})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	s2.Close()
	time.Sleep(5 * time.Millisecond)

	// A third file whose body is garbage after a valid header line.
	garbagePath := filepath.Join(dir, "garbage-session.jsonl")
	garbageHeader := Record{Seq: 1, At: time.Now(), Kind: "header", Header: &Header{ID: "garbage-session", CWD: "/garbage"}}
	hdrBytes, _ := json.Marshal(garbageHeader)
	content := string(hdrBytes) + "\nnot valid json at all{{{\n"
	if err := os.WriteFile(garbagePath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	headers, err := List(dir)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(headers) != 3 {
		t.Fatalf("len(headers) = %d, want 3", len(headers))
	}
	// Newest first: garbage-session was written last.
	if headers[0].CWD != "/garbage" {
		t.Errorf("headers[0].CWD = %q, want /garbage (newest)", headers[0].CWD)
	}
	if headers[1].CWD != "/second" {
		t.Errorf("headers[1].CWD = %q, want /second", headers[1].CWD)
	}
	if headers[2].CWD != "/first" {
		t.Errorf("headers[2].CWD = %q, want /first (oldest)", headers[2].CWD)
	}
}

// 7. Resolve matches a full id and a short prefix, and errors on an ambiguous
// or unknown prefix.
func TestResolve_FullAndPrefixMatch(t *testing.T) {
	dir := t.TempDir()

	s1, err := NewStore(dir, Header{ID: "abc123def456"})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	s1.Close()

	s2, err := NewStore(dir, Header{ID: "abc999zzzzzz"})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	s2.Close()

	// Full id match.
	path, err := Resolve(dir, "abc123def456")
	if err != nil {
		t.Fatalf("Resolve(full) error = %v", err)
	}
	if filepath.Base(path) != "abc123def456.jsonl" {
		t.Errorf("Resolve(full) = %q, want abc123def456.jsonl", path)
	}

	// Unambiguous 6-char prefix.
	path, err = Resolve(dir, "abc123")
	if err != nil {
		t.Fatalf("Resolve(prefix) error = %v", err)
	}
	if filepath.Base(path) != "abc123def456.jsonl" {
		t.Errorf("Resolve(prefix) = %q, want abc123def456.jsonl", path)
	}

	// Ambiguous prefix: both ids start with "abc".
	if _, err := Resolve(dir, "abc"); err == nil {
		t.Error("Resolve(ambiguous) error = nil, want an error")
	}

	// Unknown prefix.
	if _, err := Resolve(dir, "zzzznope"); err == nil {
		t.Error("Resolve(unknown) error = nil, want an error")
	}
}

// 8. Concurrency: many goroutines calling Append and Event concurrently.
// Every record must be present and every Seq unique. Run with -race.
func TestStore_ConcurrentAppendAndEvent(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, Header{})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			s.Append(&schema.Message{Role: schema.User, Content: "concurrent message"})
			s.Event("notice", "concurrent event")
		}(i)
	}
	wg.Wait()

	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil after a clean concurrent run", err)
	}

	recs := readRawRecords(t, s.Path())
	// 1 header + n messages + n events
	wantTotal := 1 + 2*n
	if len(recs) != wantTotal {
		t.Fatalf("records = %d, want %d", len(recs), wantTotal)
	}

	seen := make(map[int]bool, len(recs))
	for _, r := range recs {
		if seen[r.Seq] {
			t.Fatalf("duplicate Seq %d", r.Seq)
		}
		seen[r.Seq] = true
	}
	if len(seen) != wantTotal {
		t.Errorf("unique seqs = %d, want %d", len(seen), wantTotal)
	}
}

// 9. Err() reports a write failure, and a failed write never panics -- a
// recording failure must never break a session.
func TestStore_ErrAfterCloseDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, Header{})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Append() panicked after Close(): %v", r)
			}
		}()
		s.Append(&schema.Message{Role: schema.User, Content: "after close"})
	}()

	if s.Err() == nil {
		t.Error("Err() = nil, want a write error recorded after writing to a closed file")
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Event() panicked after Close(): %v", r)
			}
		}()
		s.Event("notice", "after close")
	}()
}
