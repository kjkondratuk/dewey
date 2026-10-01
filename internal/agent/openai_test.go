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

	// seen records the exact message slice handed to each Stream call, which
	// is the only way to assert what the model was actually sent as opposed
	// to what the agent kept in history.
	seen [][]*schema.Message
}

func (f *fakeStreamModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	panic("not used")
}

func (f *fakeStreamModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	f.seen = append(f.seen, in)
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

type fakeSystemProvider struct {
	text  string
	calls int
}

func (f *fakeSystemProvider) System(ctx context.Context) (*schema.Message, error) {
	f.calls++
	if f.text == "" {
		return nil, nil
	}
	return &schema.Message{Role: schema.System, Content: f.text}, nil
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

	var denial *schema.Message
	for _, msg := range agt.history {
		if msg.Role == schema.Tool && msg.Content == deniedToolResult {
			denial = msg
		}
	}
	if denial == nil {
		t.Fatal("expected a denial tool message in history")
	}
	// Name is the field the OpenAI adapter actually serializes; ToolName
	// alone would be dropped on the wire.
	if denial.Name != "do_thing" {
		t.Errorf("denial Name = %q, want %q", denial.Name, "do_thing")
	}
	if denial.ToolName != "do_thing" {
		t.Errorf("denial ToolName = %q, want %q", denial.ToolName, "do_thing")
	}
}

// A denial and a genuinely-unavailable tool must not look alike to the model:
// conflating them is what made it report being unable to act at all.
func TestDenialAndUnknownToolResultsAreDistinguishable(t *testing.T) {
	unknown := "error: unknown tool do_thing"

	if strings.HasPrefix(deniedToolResult, "error:") {
		t.Errorf("deniedToolResult wears the same error: prefix as a capability failure: %q", deniedToolResult)
	}
	if strings.HasPrefix(unknown, "error:") == strings.HasPrefix(deniedToolResult, "error:") {
		t.Error("denial and unknown-tool results are not distinguishable by shape")
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

func TestRun_CancelDuringCompactionRollsBackHistory(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{}}
	fakeSumm := &fakeSummarizer{err: context.Canceled}

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, nil, 10, fakeSumm)
	agt.history = []*schema.Message{
		{Role: schema.User, Content: strings.Repeat("filler content ", 200)},
	}
	preRunLen := len(agt.history)

	ch, err := agt.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var events []Event
	for evt := range ch {
		events = append(events, evt)
	}

	if len(events) != 1 || events[0].Kind != "cancelled" {
		t.Fatalf("events = %+v, want exactly one cancelled event", events)
	}
	if len(agt.history) != preRunLen {
		t.Errorf("history len = %d, want %d (rolled back, as if the turn never happened)", len(agt.history), preRunLen)
	}
}

func TestRun_SkipsCompactionOnFirstTurn(t *testing.T) {
	m := &fakeStreamModel{responses: [][]*schema.Message{
		finalResponse("done"),
	}}
	fakeSumm := &fakeSummarizer{result: "should not be used"}

	// contextWindow of 1 guarantees even a tiny first message would trip
	// shouldCompact if truncateLen==0 weren't special-cased.
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, nil, 1, fakeSumm)

	ch, err := agt.Run(context.Background(), strings.Repeat("filler content ", 200))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	for _, e := range events {
		if e.Kind == "compacted" {
			t.Errorf("got compacted event on first turn (truncateLen=0): %+v", e)
		}
	}
	if fakeSumm.lastMsg != nil {
		t.Errorf("summarizer was invoked on first turn, want it skipped entirely")
	}
}

func TestDescribeArgs_UnwrapsALoneStringArgument(t *testing.T) {
	got := describeArgs(`{"command":"grep -rn foo ."}`)
	if got != "grep -rn foo ." {
		t.Errorf("describeArgs() = %q, want the bare command", got)
	}
}

func TestDescribeArgs_PreservesRealNewlines(t *testing.T) {
	got := describeArgs(`{"command":"line one\nline two"}`)
	if got != "line one\nline two" {
		t.Errorf("describeArgs() = %q, want real newlines, not escaped ones", got)
	}
	if strings.Contains(got, `\n`) {
		t.Errorf("describeArgs() = %q, still contains an escaped newline", got)
	}
}

func TestDescribeArgs_FallsBackToJSONForMultipleFields(t *testing.T) {
	got := describeArgs(`{"a":"1","b":"2"}`)
	if !strings.Contains(got, `"a"`) || !strings.Contains(got, `"b"`) {
		t.Errorf("describeArgs() = %q, want indented JSON keeping both fields", got)
	}
}

func TestDescribeArgs_FallsBackForNonStringAndMalformedInput(t *testing.T) {
	if got := describeArgs(`{"n":5}`); !strings.Contains(got, "5") {
		t.Errorf("describeArgs() = %q, want the number preserved via JSON fallback", got)
	}
	if got := describeArgs(`not json at all`); got != "not json at all" {
		t.Errorf("describeArgs() = %q, want the raw input echoed back", got)
	}
}

func TestFence_OutgrowsBackticksInTheBody(t *testing.T) {
	for _, run := range []int{0, 1, 2, 3, 4, 5, 8} {
		body := "echo '" + strings.Repeat("`", run) + "'"
		got := fence(body)

		lines := strings.Split(got, "\n")
		if len(lines) < 3 {
			t.Fatalf("fence(run=%d) = %q, want open/body/close lines", run, got)
		}
		open, close := lines[0], lines[len(lines)-1]

		// CommonMark: the closing fence must be at least as long as the
		// opening one, so the opening must strictly exceed any run inside.
		if open != close {
			t.Errorf("fence(run=%d): open %q != close %q", run, open, close)
		}
		if len(open) < 3 {
			t.Errorf("fence(run=%d): open %q shorter than three backticks", run, open)
		}
		if len(open) <= run {
			t.Errorf("fence(run=%d): open %q does not outgrow the body's run", run, open)
		}
		if !strings.Contains(got, body) {
			t.Errorf("fence(run=%d) = %q, want the body verbatim", run, got)
		}
	}
}

func TestFence_UsesThreeBackticksWhenBodyHasNone(t *testing.T) {
	got := fence("ls -la")
	if got != "```\nls -la\n```" {
		t.Errorf("fence() = %q, want a plain triple-backtick fence", got)
	}
}

func TestFormatConfirmMessage_IncludesSummaryAndBareCommand(t *testing.T) {
	c := safety.Classification{Kind: "write", Risk: safety.Dangerous, Summary: "Deletes the build output directory."}
	got := formatConfirmMessage("run_shell_command", c, `{"command":"rm -rf ./dist"}`)

	for _, want := range []string{
		"run_shell_command wants to run (WRITE) [risk: dangerous]",
		"Deletes the build output directory.",
		"rm -rf ./dist",
		"Allow? [y/n]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatConfirmMessage() = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, `{"command"`) {
		t.Errorf("formatConfirmMessage() = %q, should not show the raw JSON object", got)
	}
}

func TestFormatConfirmMessage_OmitsSummaryLineWhenEmpty(t *testing.T) {
	c := safety.Classification{Risk: safety.Moderate}
	got := formatConfirmMessage("run_shell_command", c, `{"command":"touch f"}`)

	if strings.Contains(got, "\n\n\n") {
		t.Errorf("formatConfirmMessage() = %q, left a blank gap where the summary would go", got)
	}
	if !strings.Contains(got, "touch f") || !strings.Contains(got, "Allow? [y/n]") {
		t.Errorf("formatConfirmMessage() = %q, missing the command or the prompt", got)
	}
}

func TestRun_ApprovedCallEmitsBriefInvocationAndSafeCallEmitsFull(t *testing.T) {
	// Approved: the command was already shown in the confirm prompt.
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"command":"rm -rf ./dist"}`),
		finalResponse("done"),
	}}
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "write", Risk: safety.Dangerous}}, nil, 0, nil)

	ch, err := agt.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true }) // allow

	var toolPayload string
	for _, e := range events {
		if e.Kind == "tool" {
			toolPayload = e.Payload
		}
	}
	if toolPayload != formatToolInvocationBrief() {
		t.Errorf("approved tool payload = %q, want the brief form %q", toolPayload, formatToolInvocationBrief())
	}
	if strings.Contains(toolPayload, "rm -rf ./dist") {
		t.Errorf("approved tool payload = %q, should not repeat the just-approved command", toolPayload)
	}

	// Safe (auto-run): never shown, so the command has to appear here.
	invoked2 := 0
	tl2 := newRecordingTool(t, &invoked2)
	m2 := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"command":"ls -la"}`),
		finalResponse("done"),
	}}
	agt2 := New(m2, []tool.InvokableTool{tl2}, &fakeClassifier{result: safety.Classification{Kind: "read", Risk: safety.Safe}}, nil, 0, nil)

	ch2, err := agt2.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events2 := drain(t, ch2, func(Event) bool { return true })

	var toolPayload2 string
	for _, e := range events2 {
		if e.Kind == "tool" {
			toolPayload2 = e.Payload
		}
	}
	if !strings.Contains(toolPayload2, "ls -la") {
		t.Errorf("safe tool payload = %q, want it to show the command", toolPayload2)
	}
}

func TestRun_SuccessfulToolResultCarriesToolName(t *testing.T) {
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
	drain(t, ch, func(Event) bool { return true })

	found := false
	for _, msg := range agt.history {
		if msg.Role == schema.Tool {
			found = true
			if msg.ToolName != "do_thing" {
				t.Errorf("tool result ToolName = %q, want %q", msg.ToolName, "do_thing")
			}
		}
	}
	if !found {
		t.Fatal("no tool result found in history")
	}
}

func TestPlainLine_CollapsesToOneLineAndNeutralizesBlockMarkdown(t *testing.T) {
	// A summary is model-authored text rendered inside a security prompt; it
	// must not be able to restyle the prompt asking about it.
	got := plainLine("# Totally safe\n\n> trust me")
	if strings.Contains(got, "\n") {
		t.Errorf("plainLine() = %q, want a single line", got)
	}
	if !strings.HasPrefix(got, `\#`) {
		t.Errorf("plainLine() = %q, want the leading # escaped", got)
	}
}

func TestPlainLine_LeavesOrdinaryProseAlone(t *testing.T) {
	in := "Deletes the build output directory."
	if got := plainLine(in); got != in {
		t.Errorf("plainLine(%q) = %q, want it unchanged (a letter must not be backslash-escaped)", in, got)
	}
}

func TestPlainLine_EmptyStaysEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t "} {
		if got := plainLine(in); got != "" {
			t.Errorf("plainLine(%q) = %q, want empty", in, got)
		}
	}
}

func TestFormatConfirmMessage_SummaryCannotInjectAHeading(t *testing.T) {
	c := safety.Classification{Kind: "write", Risk: safety.Dangerous, Summary: "# Safe to allow"}
	got := formatConfirmMessage("run_shell_command", c, `{"command":"rm -rf /"}`)

	if strings.Contains(got, "\n# Safe to allow") {
		t.Errorf("formatConfirmMessage() = %q, summary injected a raw heading", got)
	}
	if !strings.Contains(got, `\# Safe to allow`) {
		t.Errorf("formatConfirmMessage() = %q, want the summary escaped", got)
	}
}

func TestRun_PrependsSystemMessageWithoutStoringIt(t *testing.T) {
	sp := &fakeSystemProvider{text: "SYSTEM RULES"}
	m := &fakeStreamModel{responses: [][]*schema.Message{finalResponse("ok")}}
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, nil, 0, nil)
	agt.systemProvider = sp

	ch, err := agt.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	drain(t, ch, func(Event) bool { return true })

	if len(m.seen) == 0 {
		t.Fatal("model was never called")
	}
	sent := m.seen[0]
	if sent[0].Role != schema.System || sent[0].Content != "SYSTEM RULES" {
		t.Errorf("first message sent = %+v, want the system message", sent[0])
	}
	for _, msg := range agt.history {
		if msg.Role == schema.System {
			t.Error("system message leaked into history — compaction would summarize it away")
		}
	}
}

func TestRun_SystemProviderIsConsultedEveryStream(t *testing.T) {
	// A memory saved mid-turn has to affect the very next model call.
	sp := &fakeSystemProvider{text: "RULES"}
	tl := newRecordingTool(t, new(int))
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
		finalResponse("done"),
	}}
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, nil, 0, nil)
	agt.systemProvider = sp

	ch, _ := agt.Run(context.Background(), "go")
	drain(t, ch, func(Event) bool { return true })

	if sp.calls < 2 {
		t.Errorf("system provider consulted %d times, want once per Stream call", sp.calls)
	}
}

func TestRun_NilSystemProviderSendsHistoryUnchanged(t *testing.T) {
	m := &fakeStreamModel{responses: [][]*schema.Message{finalResponse("ok")}}
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, nil, 0, nil)

	ch, _ := agt.Run(context.Background(), "hello")
	drain(t, ch, func(Event) bool { return true })

	if m.seen[0][0].Role == schema.System {
		t.Error("a system message appeared with no provider configured")
	}
}

// Asserting that the provider still returns its own text proves nothing: that
// holds even if compact() ate the message, if messages() stopped prepending
// it, or if WithSystemPrompt were a no-op. Drive a real compaction and inspect
// what actually reached the model.
func TestRun_SystemMessageSurvivesARealCompaction(t *testing.T) {
	sp := &fakeSystemProvider{text: "SYSTEM RULES"}
	m := &fakeStreamModel{responses: [][]*schema.Message{finalResponse("ok")}}

	// A tiny context window guarantees the seeded history trips the threshold.
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, nil, 10,
		&fakeSummarizer{result: "the summary"})
	agt.systemProvider = sp
	agt.history = []*schema.Message{
		{Role: schema.User, Content: strings.Repeat("filler content ", 200)},
		{Role: schema.Assistant, Content: strings.Repeat("more filler ", 200)},
	}

	ch, err := agt.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	compacted := false
	for _, e := range events {
		if e.Kind == "compacted" {
			compacted = true
		}
	}
	if !compacted {
		t.Fatal("compaction never fired; this test would prove nothing")
	}
	if len(m.seen) == 0 {
		t.Fatal("model was never called")
	}

	sent := m.seen[len(m.seen)-1]
	if sent[0].Role != schema.System || sent[0].Content != "SYSTEM RULES" {
		t.Errorf("first message after compaction = %+v, want the system message intact", sent[0])
	}
	// The compaction summary is itself a system-role message, so confirm the
	// agent's own instructions weren't what got replaced.
	if len(sent) < 2 || !strings.Contains(sent[1].Content, "the summary") {
		t.Errorf("second message = %+v, want the compaction summary", sent[1])
	}
	for _, msg := range agt.history {
		if msg.Role == schema.System && msg.Content == "SYSTEM RULES" {
			t.Error("the system prompt leaked into history, where the next compaction would consume it")
		}
	}
}

func TestCompact_DoesNotTouchTheSystemMessage(t *testing.T) {
	sp := &fakeSystemProvider{text: "RULES"}
	agt := New(nil, nil, nil, nil, 1000, &fakeSummarizer{result: "summary"})
	agt.systemProvider = sp
	agt.history = []*schema.Message{
		{Role: schema.User, Content: "one"},
		{Role: schema.Assistant, Content: "two"},
	}

	if err := agt.compact(context.Background(), 2); err != nil {
		t.Fatalf("compact() error = %v", err)
	}

	sys, _ := sp.System(context.Background())
	if sys == nil || sys.Content != "RULES" {
		t.Error("system message was affected by compaction")
	}
}
