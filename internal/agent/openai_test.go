package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"github.com/kjkondratuk/dewey/internal/safety"
)

type fakeStreamModel struct {
	responses [][]*schema.Message
	calls     int
}

func (f *fakeStreamModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	panic("not used")
}

func (f *fakeStreamModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	resp := f.responses[f.calls]
	f.calls++
	return schema.StreamReaderFromArray(resp), nil
}

func (f *fakeStreamModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return f, nil
}

type fakeClassifier struct {
	result safety.Classification
}

func (f *fakeClassifier) Classify(ctx context.Context, name, desc, argsJSON string) (safety.Classification, error) {
	return f.result, nil
}

type panicClassifier struct{}

func (panicClassifier) Classify(ctx context.Context, name, desc, argsJSON string) (safety.Classification, error) {
	panic("classifier should not be called when an override is set")
}

type fakeSummarizer struct {
	result  string
	err     error
	lastMsg []*schema.Message
}

func (f *fakeSummarizer) Summarize(ctx context.Context, messages []*schema.Message) (string, error) {
	f.lastMsg = messages
	if f.err != nil {
		return "", f.err
	}
	return f.result, nil
}

type echoInput struct {
	X string `json:"x"`
}

func newRecordingTool(t *testing.T, invoked *int) tool.InvokableTool {
	t.Helper()
	tl, err := utils.InferTool("do_thing", "does a thing", func(ctx context.Context, in echoInput) (string, error) {
		*invoked++
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("InferTool() error = %v", err)
	}
	return tl
}

func toolCallResponse(name, args string) []*schema.Message {
	return []*schema.Message{{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       "call-1",
			Type:     "function",
			Function: schema.FunctionCall{Name: name, Arguments: args},
		}},
	}}
}

func finalResponse(content string) []*schema.Message {
	return []*schema.Message{{Role: schema.Assistant, Content: content}}
}

func drain(t *testing.T, ch <-chan Event, respond func(Event) bool) []Event {
	t.Helper()
	var events []Event
	for evt := range ch {
		events = append(events, evt)
		if evt.Kind == "confirm" {
			evt.Confirm <- respond(evt)
		}
	}
	return events
}

func TestRun_SafeToolRunsWithoutConfirmation(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
		finalResponse("done"),
	}}

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "read", Risk: safety.Safe}}, nil, 0, nil)

	ch, err := agt.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	if invoked != 1 {
		t.Errorf("tool invoked %d times, want 1", invoked)
	}
	for _, e := range events {
		if e.Kind == "confirm" {
			t.Fatalf("unexpected confirm event for a safe tool: %+v", e)
		}
	}
	if events[len(events)-1].Kind != "done" {
		t.Errorf("last event = %+v, want done", events[len(events)-1])
	}
}

func TestRun_OverrideSkipsClassifierAndAllowedDangerousRuns(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
		finalResponse("done"),
	}}

	agt := New(m, []tool.InvokableTool{tl}, panicClassifier{}, safety.RiskOverrides{"do_thing": safety.Dangerous}, 0, nil)

	ch, err := agt.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	sawConfirm := false
	events := drain(t, ch, func(e Event) bool {
		sawConfirm = true
		return true // allow
	})

	if !sawConfirm {
		t.Fatal("expected a confirm event for a dangerous tool")
	}
	if invoked != 1 {
		t.Errorf("tool invoked %d times, want 1", invoked)
	}
	if events[len(events)-1].Kind != "done" {
		t.Errorf("last event = %+v, want done", events[len(events)-1])
	}
}

func TestRun_DenialSkipsToolAndFeedsHistoryBack(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
		finalResponse("okay, skipping that"),
	}}

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "write", Risk: safety.Dangerous}}, nil, 0, nil)

	ch, err := agt.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return false }) // deny

	if invoked != 0 {
		t.Errorf("tool invoked %d times, want 0 (denied)", invoked)
	}
	if events[len(events)-1].Kind != "done" {
		t.Errorf("last event = %+v, want done", events[len(events)-1])
	}

	found := false
	for _, msg := range agt.history {
		if msg.Role == schema.Tool && msg.Content == "error: user denied permission for this action" {
			found = true
		}
	}
	if !found {
		t.Error("expected a denial tool message in history")
	}
}

func TestRun_CancelDuringConfirmationDropsTheTurn(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
	}}

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "write", Risk: safety.Dangerous}}, nil, 0, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := agt.Run(ctx, "go")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var kinds []string
	for evt := range ch {
		kinds = append(kinds, evt.Kind)
		if evt.Kind == "confirm" {
			cancel() // do not respond; the agent must unblock via ctx.Done()
		}
	}

	if invoked != 0 {
		t.Errorf("tool invoked %d times, want 0 (cancelled)", invoked)
	}
	if len(agt.history) != 0 {
		t.Errorf("history = %+v, want empty (turn dropped)", agt.history)
	}
	if kinds[len(kinds)-1] != "cancelled" {
		t.Errorf("last event kind = %q, want cancelled", kinds[len(kinds)-1])
	}
}

func TestShouldCompact(t *testing.T) {
	agt := New(nil, nil, nil, nil, 1000, nil)

	if agt.shouldCompact(899) {
		t.Error("shouldCompact(899) with window 1000 = true, want false (< 90%)")
	}
	if !agt.shouldCompact(900) {
		t.Error("shouldCompact(900) with window 1000 = false, want true (>= 90%)")
	}
}

func TestShouldCompact_DisabledWhenContextWindowIsZero(t *testing.T) {
	agt := New(nil, nil, nil, nil, 0, nil)

	if agt.shouldCompact(1_000_000) {
		t.Error("shouldCompact() with contextWindow 0 = true, want false (disabled)")
	}
}

func TestEstimateTokens_UsesProviderUsageAsBaseline(t *testing.T) {
	agt := New(nil, nil, nil, nil, 1000, nil)
	agt.history = []*schema.Message{{Role: schema.User, Content: "hi"}}

	full := &schema.Message{
		Role:         schema.Assistant,
		Content:      "hello",
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{TotalTokens: 500}},
	}
	agt.history = append(agt.history, full)
	agt.recordUsage(full)

	if agt.lastKnownTokens != 500 {
		t.Errorf("lastKnownTokens = %d, want 500", agt.lastKnownTokens)
	}
	if agt.lastKnownAt != len(agt.history) {
		t.Errorf("lastKnownAt = %d, want %d", agt.lastKnownAt, len(agt.history))
	}

	// Anything appended after the recorded point should only add an
	// estimate on top of the trusted baseline, not recompute from scratch.
	agt.history = append(agt.history, &schema.Message{Role: schema.User, Content: "a short follow-up"})
	estimated := agt.estimateTokens()
	if estimated <= 500 {
		t.Errorf("estimateTokens() = %d, want > 500 (baseline + delta)", estimated)
	}
}

func TestRun_CompactsWhenHistoryExceedsThreshold(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		finalResponse("done"),
	}}
	fakeSumm := &fakeSummarizer{result: "the short summary"}

	// A tiny context window guarantees the pre-seeded history exceeds 90% of it.
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, nil, 10, fakeSumm)
	agt.history = []*schema.Message{
		{Role: schema.User, Content: strings.Repeat("filler content ", 200)},
		{Role: schema.Assistant, Content: strings.Repeat("more filler content ", 200)},
	}
	preCompactLen := len(agt.history)

	ch, err := agt.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	if events[0].Kind != "compacted" {
		t.Fatalf("first event = %+v, want compacted", events[0])
	}
	if events[len(events)-1].Kind != "done" {
		t.Errorf("last event = %+v, want done", events[len(events)-1])
	}

	if len(fakeSumm.lastMsg) != preCompactLen {
		t.Errorf("summarizer received %d messages, want %d (pre-existing history, not the new user turn)", len(fakeSumm.lastMsg), preCompactLen)
	}
	if agt.history[0].Role != schema.System {
		t.Errorf("history[0].Role = %v, want schema.System", agt.history[0].Role)
	}
	if !strings.Contains(agt.history[0].Content, "the short summary") {
		t.Errorf("history[0].Content = %q, want it to contain the summary", agt.history[0].Content)
	}
	if agt.history[1].Content != "continue" {
		t.Errorf("history[1].Content = %q, want %q (current turn preserved)", agt.history[1].Content, "continue")
	}
}

func TestCompact_ResetsUsageBaseline(t *testing.T) {
	agt := New(nil, nil, nil, nil, 1000, &fakeSummarizer{result: "summary"})
	agt.history = []*schema.Message{
		{Role: schema.User, Content: "old message 1"},
		{Role: schema.Assistant, Content: "old message 2"},
	}
	agt.lastKnownTokens = 500
	agt.lastKnownAt = 2

	if err := agt.compact(context.Background(), 2); err != nil {
		t.Fatalf("compact() error = %v", err)
	}

	if agt.lastKnownTokens != 0 {
		t.Errorf("lastKnownTokens = %d, want 0 (reset after compaction)", agt.lastKnownTokens)
	}
	if agt.lastKnownAt != 0 {
		t.Errorf("lastKnownAt = %d, want 0 (reset after compaction)", agt.lastKnownAt)
	}
}

func TestCompact_NoOpWhenTruncateLenIsZero(t *testing.T) {
	agt := New(nil, nil, nil, nil, 1000, &fakeSummarizer{result: "should not be used"})
	agt.history = []*schema.Message{{Role: schema.User, Content: "only message"}}

	if err := agt.compact(context.Background(), 0); err != nil {
		t.Fatalf("compact() error = %v", err)
	}

	if len(agt.history) != 1 || agt.history[0].Content != "only message" {
		t.Errorf("history = %+v, want unchanged (compact should no-op when truncateLen is 0)", agt.history)
	}
}

func TestRun_CompactionFailureSurfacesAsError(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{}}
	fakeSumm := &fakeSummarizer{err: errors.New("model unavailable")}

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, nil, 10, fakeSumm)
	agt.history = []*schema.Message{
		{Role: schema.User, Content: strings.Repeat("filler content ", 200)},
	}

	ch, err := agt.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var events []Event
	for evt := range ch {
		events = append(events, evt)
	}

	if len(events) != 1 || events[0].Kind != "error" {
		t.Fatalf("events = %+v, want exactly one error event", events)
	}
}
