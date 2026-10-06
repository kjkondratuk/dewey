package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

// 1. A session with a user message, an assistant message and a tool result
// yields FirstUserMessage of the user's text and MessageCount 3.
func TestSummaries_FirstUserMessageAndCount(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, Header{CWD: "/work"})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	s.Append(&schema.Message{Role: schema.User, Content: "what's the weather?"})
	s.Append(&schema.Message{Role: schema.Assistant, Content: "let me check"})
	tool := schema.ToolMessage("sunny", "call-1", schema.WithToolName("get_weather"))
	s.Append(tool)
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	sums, err := Summaries(dir)
	if err != nil {
		t.Fatalf("Summaries() error = %v", err)
	}
	if len(sums) != 1 {
		t.Fatalf("len(sums) = %d, want 1", len(sums))
	}
	if sums[0].FirstUserMessage != "what's the weather?" {
		t.Errorf("FirstUserMessage = %q, want %q", sums[0].FirstUserMessage, "what's the weather?")
	}
	if sums[0].MessageCount != 3 {
		t.Errorf("MessageCount = %d, want 3", sums[0].MessageCount)
	}
}

// 2. The first user message wins when there are several.
func TestSummaries_FirstUserMessageWinsAmongSeveral(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, Header{})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	s.Append(&schema.Message{Role: schema.User, Content: "first question"})
	s.Append(&schema.Message{Role: schema.Assistant, Content: "an answer"})
	s.Append(&schema.Message{Role: schema.User, Content: "second question"})
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	sums, err := Summaries(dir)
	if err != nil {
		t.Fatalf("Summaries() error = %v", err)
	}
	if len(sums) != 1 {
		t.Fatalf("len(sums) = %d, want 1", len(sums))
	}
	if sums[0].FirstUserMessage != "first question" {
		t.Errorf("FirstUserMessage = %q, want %q", sums[0].FirstUserMessage, "first question")
	}
}

// 3. A multi-line user message is collapsed to a single spaced line.
func TestSummaries_MultilineMessageCollapsedToSingleLine(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, Header{})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	s.Append(&schema.Message{Role: schema.User, Content: "line one\n\n  line two\tline three"})
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	sums, err := Summaries(dir)
	if err != nil {
		t.Fatalf("Summaries() error = %v", err)
	}
	if len(sums) != 1 {
		t.Fatalf("len(sums) = %d, want 1", len(sums))
	}
	want := "line one line two line three"
	if sums[0].FirstUserMessage != want {
		t.Errorf("FirstUserMessage = %q, want %q", sums[0].FirstUserMessage, want)
	}
}

// 4. A session with no user message yields an empty FirstUserMessage and the
// correct count.
func TestSummaries_NoUserMessage(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, Header{})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	s.Append(&schema.Message{Role: schema.Assistant, Content: "unsolicited greeting"})
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	sums, err := Summaries(dir)
	if err != nil {
		t.Fatalf("Summaries() error = %v", err)
	}
	if len(sums) != 1 {
		t.Fatalf("len(sums) = %d, want 1", len(sums))
	}
	if sums[0].FirstUserMessage != "" {
		t.Errorf("FirstUserMessage = %q, want empty", sums[0].FirstUserMessage)
	}
	if sums[0].MessageCount != 1 {
		t.Errorf("MessageCount = %d, want 1", sums[0].MessageCount)
	}
}

// 5. Newest-first ordering across three files.
func TestSummaries_NewestFirst(t *testing.T) {
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

	s3, err := NewStore(dir, Header{CWD: "/third"})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	s3.Close()

	sums, err := Summaries(dir)
	if err != nil {
		t.Fatalf("Summaries() error = %v", err)
	}
	if len(sums) != 3 {
		t.Fatalf("len(sums) = %d, want 3", len(sums))
	}
	if sums[0].CWD != "/third" || sums[1].CWD != "/second" || sums[2].CWD != "/first" {
		t.Errorf("order = [%s %s %s], want [/third /second /first]", sums[0].CWD, sums[1].CWD, sums[2].CWD)
	}
}

// 6. A file with a valid header and a corrupt body still appears, with its
// header intact (ported from the old List test).
func TestSummaries_CorruptBodyStillYieldsHeader(t *testing.T) {
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

	garbagePath := filepath.Join(dir, "garbage-session.jsonl")
	garbageHeader := Record{Seq: 1, At: time.Now(), Kind: "header", Header: &Header{ID: "garbage-session", CWD: "/garbage", StartedAt: time.Now()}}
	hdrBytes, _ := json.Marshal(garbageHeader)
	content := string(hdrBytes) + "\nnot valid json at all{{{\n"
	if err := os.WriteFile(garbagePath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	sums, err := Summaries(dir)
	if err != nil {
		t.Fatalf("Summaries() error = %v", err)
	}
	if len(sums) != 3 {
		t.Fatalf("len(sums) = %d, want 3", len(sums))
	}
	if sums[0].CWD != "/garbage" {
		t.Errorf("sums[0].CWD = %q, want /garbage (newest)", sums[0].CWD)
	}
	if sums[1].CWD != "/second" {
		t.Errorf("sums[1].CWD = %q, want /second", sums[1].CWD)
	}
	if sums[2].CWD != "/first" {
		t.Errorf("sums[2].CWD = %q, want /first (oldest)", sums[2].CWD)
	}
}

// 7. A missing directory returns nil, nil.
func TestSummaries_MissingDirectory(t *testing.T) {
	sums, err := Summaries(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("Summaries() error = %v, want nil", err)
	}
	if sums != nil {
		t.Errorf("Summaries() = %v, want nil", sums)
	}
}
