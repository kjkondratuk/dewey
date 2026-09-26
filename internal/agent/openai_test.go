package agent

import (
	"context"
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

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "read", Risk: safety.Safe}}, nil)

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

	agt := New(m, []tool.InvokableTool{tl}, panicClassifier{}, safety.RiskOverrides{"do_thing": safety.Dangerous})

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

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "write", Risk: safety.Dangerous}}, nil)

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

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "write", Risk: safety.Dangerous}}, nil)

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
