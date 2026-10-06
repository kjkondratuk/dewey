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

	// Each needs a message: a session with none is deliberately not listed,
	// so a header-only fixture would test the filter rather than the ordering.
	s1, err := NewStore(dir, Header{CWD: "/first"})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	s1.Append(&schema.Message{Role: schema.User, Content: "first request here"})
	s1.Close()
	time.Sleep(5 * time.Millisecond)

	s2, err := NewStore(dir, Header{CWD: "/second"})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	s2.Append(&schema.Message{Role: schema.User, Content: "second request here"})
	s2.Close()
	time.Sleep(5 * time.Millisecond)

	s3, err := NewStore(dir, Header{CWD: "/third"})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	s3.Append(&schema.Message{Role: schema.User, Content: "third request here"})
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
func TestSummaries_CorruptTailStillYieldsTheSession(t *testing.T) {
	dir := t.TempDir()

	s1, err := NewStore(dir, Header{CWD: "/first"})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	s1.Append(&schema.Message{Role: schema.User, Content: "first request here"})
	s1.Close()
	time.Sleep(5 * time.Millisecond)

	s2, err := NewStore(dir, Header{CWD: "/second"})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	s2.Append(&schema.Message{Role: schema.User, Content: "second request here"})
	s2.Close()
	time.Sleep(5 * time.Millisecond)

	// A session killed mid-write is valid records followed by a partial line,
	// which is the shape Load is built to tolerate -- and the shape worth
	// asserting, since those are the sessions most worth recovering.
	garbagePath := filepath.Join(dir, "garbage-session.jsonl")
	garbageHeader := Record{Seq: 1, At: time.Now(), Kind: "header", Header: &Header{ID: "garbage-session", CWD: "/garbage", StartedAt: time.Now()}}
	hdrBytes, _ := json.Marshal(garbageHeader)
	msg := Record{Seq: 2, At: time.Now(), Kind: "message", Role: "user", Content: "investigate the timeout"}
	msgBytes, _ := json.Marshal(msg)
	content := string(hdrBytes) + "\n" + string(msgBytes) + "\nnot valid json at all{{{\n"
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

func TestSummaries_SkipsAGreetingForTheFirstRealRequest(t *testing.T) {
	// Measured against real transcripts: the user opens with "hello", then
	// says what they want. Labelling an 18-message code review "hello" is
	// accurate and useless, which is the whole problem the summary exists to
	// solve.
	dir := t.TempDir()
	st, err := NewStore(dir, Header{CWD: "/p"})
	if err != nil {
		t.Fatal(err)
	}
	st.Append(&schema.Message{Role: schema.User, Content: "hello"})
	st.Append(&schema.Message{Role: schema.Assistant, Content: "Hi."})
	st.Append(&schema.Message{Role: schema.User, Content: "review the code in this project"})
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	sums, err := Summaries(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 1 {
		t.Fatalf("got %d summaries, want 1", len(sums))
	}
	if got := sums[0].FirstUserMessage; got != "review the code in this project" {
		t.Errorf("FirstUserMessage = %q, want the first substantive request", got)
	}
}

func TestSummaries_FallsBackToAShortOpeningWhenItIsAllThereIs(t *testing.T) {
	// "hello" is a poor label but an honest one when the session contains
	// nothing better. Never show empty where text exists.
	dir := t.TempDir()
	st, err := NewStore(dir, Header{CWD: "/p"})
	if err != nil {
		t.Fatal(err)
	}
	st.Append(&schema.Message{Role: schema.User, Content: "hello?"})
	st.Append(&schema.Message{Role: schema.User, Content: "continue"})
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	sums, err := Summaries(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := sums[0].FirstUserMessage; got != "hello?" {
		t.Errorf("FirstUserMessage = %q, want the opening message as a fallback", got)
	}
}

func TestSummaries_OmitsSessionsWithNoMessages(t *testing.T) {
	// A launch that never got a prompt records a header and nothing else.
	// Resuming it yields empty history, which is a fresh session, so listing
	// it is noise in the one place that must stay scannable.
	dir := t.TempDir()
	empty, err := NewStore(dir, Header{CWD: "/p"})
	if err != nil {
		t.Fatal(err)
	}
	empty.Event("notice", "started")
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}

	real, err := NewStore(dir, Header{CWD: "/p"})
	if err != nil {
		t.Fatal(err)
	}
	real.Append(&schema.Message{Role: schema.User, Content: "do something useful"})
	if err := real.Close(); err != nil {
		t.Fatal(err)
	}

	sums, err := Summaries(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 1 {
		t.Fatalf("got %d summaries, want only the one with messages", len(sums))
	}
	if sums[0].ID != real.ID() {
		t.Errorf("kept %q, want the session that has messages", sums[0].ID)
	}
}
