package agent

import (
	"context"
	"errors"
	"slices"
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

	// errs, when non-nil, supplies a per-call error returned instead of the
	// matching entry in responses. A nil entry means that call succeeds, so a
	// model that fails once and then recovers is []error{err, nil}.
	errs []error

	// seen records the exact message slice handed to each Stream call, which
	// is the only way to assert what the model was actually sent as opposed
	// to what the agent kept in history.
	seen [][]*schema.Message

	// streamReaders, when set for a call index, is returned instead of a
	// reader built from responses[call]. This is how a test models a
	// provider that fails *mid-stream*, after already yielding chunks --
	// something StreamReaderFromArray cannot do, since it never produces
	// anything but io.EOF.
	streamReaders []*schema.StreamReader[*schema.Message]
}

func (f *fakeStreamModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	panic("not used")
}

func (f *fakeStreamModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	f.seen = append(f.seen, in)
	call := f.calls
	f.calls++
	if call < len(f.errs) && f.errs[call] != nil {
		return nil, f.errs[call]
	}
	if call < len(f.streamReaders) && f.streamReaders[call] != nil {
		return f.streamReaders[call], nil
	}
	resp := f.responses[call]
	return schema.StreamReaderFromArray(resp), nil
}

// pipeStream builds a StreamReader that yields chunks in order and then fails
// with err (or hits io.EOF if err is nil). Unlike schema.StreamReaderFromArray,
// which can only ever end in io.EOF, this models a provider stream that is
// cancelled partway through -- after some chunks already arrived.
func pipeStream(chunks []*schema.Message, err error) *schema.StreamReader[*schema.Message] {
	sr, sw := schema.Pipe[*schema.Message](len(chunks) + 1)
	go func() {
		defer sw.Close()
		for _, c := range chunks {
			if sw.Send(c, nil) {
				return
			}
		}
		if err != nil {
			sw.Send(nil, err)
		}
	}()
	return sr
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

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "read", Risk: safety.Safe}}, 0, nil)

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

func TestRun_DenialSkipsToolAndFeedsHistoryBack(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
		finalResponse("okay, skipping that"),
	}}

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "write", Risk: safety.Dangerous}}, 0, nil)

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

func TestShouldCompact(t *testing.T) {
	agt := New(nil, nil, nil, 1000, nil)

	if agt.shouldCompact(899) {
		t.Error("shouldCompact(899) with window 1000 = true, want false (< 90%)")
	}
	if !agt.shouldCompact(900) {
		t.Error("shouldCompact(900) with window 1000 = false, want true (>= 90%)")
	}
}

func TestShouldCompact_DisabledWhenContextWindowIsZero(t *testing.T) {
	agt := New(nil, nil, nil, 0, nil)

	if agt.shouldCompact(1_000_000) {
		t.Error("shouldCompact() with contextWindow 0 = true, want false (disabled)")
	}
}

func TestEstimateTokens_UsesProviderUsageAsBaseline(t *testing.T) {
	agt := New(nil, nil, nil, 1000, nil)
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
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 10, fakeSumm)
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
	agt := New(nil, nil, nil, 1000, &fakeSummarizer{result: "summary"})
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
	agt := New(nil, nil, nil, 1000, &fakeSummarizer{result: "should not be used"})
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

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 10, fakeSumm)
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

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 10, fakeSumm)
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

	if len(events) != 1 || events[0].Kind != "cancelled" {
		t.Fatalf("events = %+v, want exactly one cancelled event", events)
	}
	// The prompt survives even though nothing ran: erasing it leaves the
	// user talking about a turn the model can no longer see.
	want := []string{"user", "user", "note"}
	if got := historyShape(agt.history); !slices.Equal(got, want) {
		t.Errorf("history shape = %v, want %v", got, want)
	}
}

func TestRun_SkipsCompactionOnFirstTurn(t *testing.T) {
	m := &fakeStreamModel{responses: [][]*schema.Message{
		finalResponse("done"),
	}}
	fakeSumm := &fakeSummarizer{result: "should not be used"}

	// contextWindow of 1 guarantees even a tiny first message would trip
	// shouldCompact if truncateLen==0 weren't special-cased.
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 1, fakeSumm)

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
	got := formatConfirmMessage("Bash", c, `{"command":"rm -rf ./dist"}`)

	for _, want := range []string{
		"Bash wants to run (WRITE) [risk: dangerous]",
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
	got := formatConfirmMessage("Bash", c, `{"command":"touch f"}`)

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
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "write", Risk: safety.Dangerous}}, 0, nil)

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
	agt2 := New(m2, []tool.InvokableTool{tl2}, &fakeClassifier{result: safety.Classification{Kind: "read", Risk: safety.Safe}}, 0, nil)

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
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "read", Risk: safety.Safe}}, 0, nil)

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
	got := formatConfirmMessage("Bash", c, `{"command":"rm -rf /"}`)

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
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 0, nil)
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
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 0, nil)
	agt.systemProvider = sp

	ch, _ := agt.Run(context.Background(), "go")
	drain(t, ch, func(Event) bool { return true })

	if sp.calls < 2 {
		t.Errorf("system provider consulted %d times, want once per Stream call", sp.calls)
	}
}

func TestRun_NilSystemProviderSendsHistoryUnchanged(t *testing.T) {
	m := &fakeStreamModel{responses: [][]*schema.Message{finalResponse("ok")}}
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 0, nil)

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
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 10,
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
	agt := New(nil, nil, nil, 1000, &fakeSummarizer{result: "summary"})
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

// noticingProvider reports notices that change between calls, mimicking a
// memory evicted for budget partway through a session.
type noticingProvider struct {
	calls   int
	notices []string
}

func (p *noticingProvider) System(ctx context.Context) (*schema.Message, error) {
	p.calls++
	if p.calls > 1 {
		p.notices = []string{"memory budget of 30 tokens exceeded; 1 memories omitted"}
	}
	return &schema.Message{Role: schema.System, Content: "RULES"}, nil
}

func (p *noticingProvider) Notices() []string { return p.notices }

// A memory evicted for budget right after the user was told "Saved." must not
// vanish silently — the startup report cannot cover something that happens
// mid-session.
func TestRun_MidSessionNoticesAreSurfaced(t *testing.T) {
	sp := &noticingProvider{}
	tl := newRecordingTool(t, new(int))
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
		finalResponse("done"),
	}}
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 0, nil)
	agt.systemProvider = sp

	ch, err := agt.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	var notices []string
	for _, e := range events {
		if e.Kind == "notice" {
			notices = append(notices, e.Payload)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("notice events = %v, want exactly one", notices)
	}
	if !strings.Contains(notices[0], "budget") {
		t.Errorf("notice = %q, want the budget message", notices[0])
	}
}

// A steady-state problem must be reported once, not before every model call.
func TestRun_UnchangedNoticesAreNotRepeated(t *testing.T) {
	sp := &fakeNoticeProvider{notices: []string{"skipped memory bad.md: invalid frontmatter"}}
	tl := newRecordingTool(t, new(int))
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
		finalResponse("done"),
	}}
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 0, nil)
	agt.systemProvider = sp

	ch, _ := agt.Run(context.Background(), "go")
	events := drain(t, ch, func(Event) bool { return true })

	count := 0
	for _, e := range events {
		if e.Kind == "notice" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("notice events = %d across two Stream calls, want 1 — a standing problem must not repeat", count)
	}
}

type fakeNoticeProvider struct{ notices []string }

func (p *fakeNoticeProvider) System(ctx context.Context) (*schema.Message, error) {
	return &schema.Message{Role: schema.System, Content: "RULES"}, nil
}
func (p *fakeNoticeProvider) Notices() []string { return p.notices }

// The reported bug, at the layer it actually bit: the classifier correctly
// identified a file-creating command as a write but rated it "safe", and the
// gate -- which only looked at the tier -- ran it with no prompt.
func TestRun_WriteRatedSafeStillAsksFirst(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
		finalResponse("done"),
	}}
	// Exactly what gemma returns for `echo hello > smoke-test.txt`.
	writeRatedSafe := &fakeClassifier{result: safety.Classification{Kind: "write", Risk: safety.Safe}}

	agt := New(m, []tool.InvokableTool{tl}, writeRatedSafe, 0, nil)
	ch, err := agt.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	sawConfirm := false
	drain(t, ch, func(Event) bool {
		sawConfirm = true
		return false // deny
	})

	if !sawConfirm {
		t.Error("no confirm event for a write rated safe — it would have run silently")
	}
	if invoked != 0 {
		t.Errorf("tool invoked %d times after denial, want 0", invoked)
	}
}

// Reads must stay fast: a confirmation on every `ls` would make the gate
// useless by training the user to approve reflexively.
func TestRun_ReadRatedSafeStillRunsWithoutAsking(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
		finalResponse("done"),
	}}
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "read", Risk: safety.Safe}}, 0, nil)

	ch, _ := agt.Run(context.Background(), "go")
	events := drain(t, ch, func(Event) bool { return true })

	for _, e := range events {
		if e.Kind == "confirm" {
			t.Fatal("confirm event for a read rated safe")
		}
	}
	if invoked != 1 {
		t.Errorf("tool invoked %d times, want 1", invoked)
	}
}

func TestIsContextOverflow(t *testing.T) {
	overflow := []string{
		"context_length_exceeded",
		"This model's maximum context length is 8192 tokens, however you requested 9000",
		"Please reduce the length of the messages.",
		"trying to keep the first 4096 tokens when context the overflow policy is set to stop",
		"prompt is too long: 300000 tokens > 200000 maximum",
		"ERROR: Context Window Exceeded",
	}
	for _, msg := range overflow {
		if !isContextOverflow(errors.New(msg)) {
			t.Errorf("isContextOverflow(%q) = false, want true", msg)
		}
	}

	notOverflow := []string{
		"connection refused",
		"401 unauthorized",
		"model not found",
		"",
		"rate limit exceeded",
	}
	for _, msg := range notOverflow {
		if isContextOverflow(errors.New(msg)) {
			t.Errorf("isContextOverflow(%q) = true, want false", msg)
		}
	}

	if isContextOverflow(nil) {
		t.Error("isContextOverflow(nil) = true, want false")
	}
}

func TestRun_ProviderContextOverflowForcesCompactionAndRetries(t *testing.T) {
	// The backstop for a believed context window that is larger than what the
	// provider will actually serve: compaction never fires on its own, so the
	// provider's rejection is the only signal that history must shrink.
	m := &fakeStreamModel{
		responses: [][]*schema.Message{nil, finalResponse("recovered")},
		errs:      []error{errors.New("context_length_exceeded"), nil},
	}
	fakeSumm := &fakeSummarizer{result: "the short summary"}

	// A huge window means shouldCompact never triggers proactively.
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 262144, fakeSumm)
	agt.history = []*schema.Message{
		{Role: schema.User, Content: "earlier question"},
		{Role: schema.Assistant, Content: "earlier answer"},
	}

	ch, err := agt.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if len(events) == 0 || events[len(events)-1].Kind != "done" {
		t.Fatalf("events = %v, want the turn to recover and finish with done", kinds)
	}
	if fakeSumm.lastMsg == nil {
		t.Error("summarizer was never called, want a forced compaction")
	}

	var compacted bool
	for _, e := range events {
		if e.Kind == "compacted" {
			compacted = true
			if !strings.Contains(strings.ToLower(e.Payload), "provider") {
				t.Errorf("compacted payload = %q, want it to explain the provider forced it", e.Payload)
			}
		}
	}
	if !compacted {
		t.Error("no compacted event emitted, want the forced compaction reported")
	}

	if m.calls != 2 {
		t.Errorf("model Stream calls = %d, want 2 (one rejected, one retried)", m.calls)
	}
}

func TestRun_ProviderContextOverflowLowersTheBelievedWindow(t *testing.T) {
	// Discovery can report a window far larger than the server will serve. A
	// rejection proves the believed window is wrong, so it must come down or
	// the next turn repeats the failure.
	m := &fakeStreamModel{
		responses: [][]*schema.Message{nil, finalResponse("recovered")},
		errs:      []error{errors.New("maximum context length is 8192 tokens"), nil},
	}
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 262144, &fakeSummarizer{result: "summary"})
	agt.history = []*schema.Message{
		{Role: schema.User, Content: strings.Repeat("filler ", 100)},
	}

	ch, err := agt.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	drain(t, ch, func(Event) bool { return true })

	if agt.contextWindow >= 262144 {
		t.Errorf("contextWindow = %d, want it lowered below the rejected 262144", agt.contextWindow)
	}
	if agt.contextWindow <= 0 {
		t.Errorf("contextWindow = %d, want a usable positive window", agt.contextWindow)
	}
}

func TestRun_ProviderContextOverflowRetriesOnlyOnce(t *testing.T) {
	m := &fakeStreamModel{
		responses: [][]*schema.Message{nil, nil},
		errs:      []error{errors.New("context_length_exceeded"), errors.New("context_length_exceeded")},
	}
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 262144, &fakeSummarizer{result: "summary"})
	agt.history = []*schema.Message{{Role: schema.User, Content: "earlier"}}

	ch, err := agt.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	last := events[len(events)-1]
	if last.Kind != "error" {
		t.Errorf("last event = %+v, want error after the retry also overflowed", last)
	}
	if m.calls != 2 {
		t.Errorf("model Stream calls = %d, want exactly 2 (no infinite retry)", m.calls)
	}
}

func TestRun_NonOverflowStreamErrorIsNotCompacted(t *testing.T) {
	m := &fakeStreamModel{
		responses: [][]*schema.Message{nil},
		errs:      []error{errors.New("connection refused")},
	}
	fakeSumm := &fakeSummarizer{result: "summary"}
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 262144, fakeSumm)
	agt.history = []*schema.Message{{Role: schema.User, Content: "earlier"}}

	ch, err := agt.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	if len(events) != 1 || events[0].Kind != "error" {
		t.Fatalf("events = %+v, want exactly one error event", events)
	}
	if fakeSumm.lastMsg != nil {
		t.Error("summarizer was called, want no compaction for an unrelated error")
	}
	if m.calls != 1 {
		t.Errorf("model Stream calls = %d, want 1 (no retry)", m.calls)
	}
}

// countingRefresher records how often it was consulted, which is how the
// once-per-session guarantee is asserted.
type countingRefresher struct {
	window int
	ok     bool
	calls  int
}

func (c *countingRefresher) refresh(ctx context.Context) (int, bool) {
	c.calls++
	return c.window, c.ok
}

func TestRun_RefreshesContextWindowAfterFirstSuccessfulCall(t *testing.T) {
	// Startup could only clamp a guess because the model was not loaded. The
	// first successful call proves it is loaded now, so the real window is
	// discoverable and should be adopted.
	m := &fakeStreamModel{responses: [][]*schema.Message{finalResponse("done")}}
	ref := &countingRefresher{window: 262144, ok: true}

	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 32768, &fakeSummarizer{result: "summary"}).
		WithContextWindowRefresher(ref.refresh)

	ch, err := agt.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	if agt.contextWindow != 262144 {
		t.Errorf("contextWindow = %d, want 262144 adopted from the refresh", agt.contextWindow)
	}
	if ref.calls != 1 {
		t.Errorf("refresher calls = %d, want 1", ref.calls)
	}

	var noticed bool
	for _, e := range events {
		if e.Kind == "notice" && strings.Contains(e.Payload, "262144") {
			noticed = true
		}
	}
	if !noticed {
		t.Error("no notice mentioning the new window, want the change reported")
	}
}

func TestRun_RefreshesContextWindowOnlyOncePerSession(t *testing.T) {
	m := &fakeStreamModel{responses: [][]*schema.Message{
		finalResponse("first"),
		finalResponse("second"),
	}}
	ref := &countingRefresher{window: 131072, ok: true}

	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 32768, &fakeSummarizer{result: "summary"}).
		WithContextWindowRefresher(ref.refresh)

	for _, input := range []string{"one", "two"} {
		ch, err := agt.Run(context.Background(), input)
		if err != nil {
			t.Fatalf("Run(%q) error = %v", input, err)
		}
		drain(t, ch, func(Event) bool { return true })
	}

	if ref.calls != 1 {
		t.Errorf("refresher calls = %d, want 1 across two turns", ref.calls)
	}
}

func TestRun_RefreshDoesNotRaiseAWindowTheProviderAlreadyRejected(t *testing.T) {
	// A provider rejection is direct evidence about what the server will
	// serve; discovery is an inference. Letting the refresh undo the lowered
	// window would walk straight back into the overflow it just recovered from.
	m := &fakeStreamModel{
		responses: [][]*schema.Message{nil, finalResponse("recovered")},
		errs:      []error{errors.New("context_length_exceeded"), nil},
	}
	ref := &countingRefresher{window: 262144, ok: true}

	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 262144, &fakeSummarizer{result: "summary"}).
		WithContextWindowRefresher(ref.refresh)
	agt.history = []*schema.Message{{Role: schema.User, Content: "earlier"}}

	ch, err := agt.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	drain(t, ch, func(Event) bool { return true })

	if agt.contextWindow >= 262144 {
		t.Errorf("contextWindow = %d, want it to stay lowered after the rejection", agt.contextWindow)
	}
}

func TestRun_RefreshReportingNothingLeavesTheWindowAlone(t *testing.T) {
	m := &fakeStreamModel{responses: [][]*schema.Message{finalResponse("done")}}
	ref := &countingRefresher{ok: false}

	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 32768, &fakeSummarizer{result: "summary"}).
		WithContextWindowRefresher(ref.refresh)

	ch, err := agt.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	if agt.contextWindow != 32768 {
		t.Errorf("contextWindow = %d, want 32768 unchanged", agt.contextWindow)
	}
	for _, e := range events {
		if e.Kind == "notice" {
			t.Errorf("unexpected notice %q, want silence when there is nothing to report", e.Payload)
		}
	}
}

func TestRun_NilRefresherIsHarmless(t *testing.T) {
	m := &fakeStreamModel{responses: [][]*schema.Message{finalResponse("done")}}
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 32768, &fakeSummarizer{result: "summary"})

	ch, err := agt.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	if events[len(events)-1].Kind != "done" {
		t.Errorf("last event = %+v, want done", events[len(events)-1])
	}
	if agt.contextWindow != 32768 {
		t.Errorf("contextWindow = %d, want 32768 unchanged", agt.contextWindow)
	}
}

// recordedCall is one call the fakeRecorder observed, either an Append or an
// Event.
type recordedCall struct {
	kind   string // "append" or "event"
	msg    *schema.Message
	event  string
	detail string
}

// fakeRecorder is a Recorder that remembers every call, in order, so tests
// can assert both what was recorded and the sequence it happened in. Calls
// only ever originate from the single goroutine inside Run, and tests only
// inspect calls after draining that goroutine's channel to completion, so no
// locking is needed -- the channel close is the synchronization point.
type fakeRecorder struct {
	calls []recordedCall
}

func (f *fakeRecorder) Append(msg *schema.Message) {
	f.calls = append(f.calls, recordedCall{kind: "append", msg: msg})
}

func (f *fakeRecorder) Event(event, detail string) {
	f.calls = append(f.calls, recordedCall{kind: "event", event: event, detail: detail})
}

func (f *fakeRecorder) kinds() []string {
	kinds := make([]string, len(f.calls))
	for i, c := range f.calls {
		if c.kind == "append" {
			kinds[i] = "append:" + string(c.msg.Role)
		} else {
			kinds[i] = "event:" + c.event
		}
	}
	return kinds
}

// A normal turn with one tool call must record, in order: the user message,
// a request event before each Stream call, the assistant message (including
// the tool-call response), the tool result, and the final assistant message.
func TestRun_RecordsConversationInOrder(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
		finalResponse("done"),
	}}
	rec := &fakeRecorder{}
	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 0, nil).
		WithRecorder(rec)

	ch, err := agt.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	drain(t, ch, func(Event) bool { return true })

	want := []string{
		"append:user",
		"event:request",
		"append:assistant",
		"append:tool",
		"event:request",
		"append:assistant",
	}
	got := rec.kinds()
	if len(got) != len(want) {
		t.Fatalf("recorded calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("calls[%d] = %q, want %q (full sequence: %v)", i, got[i], want[i], got)
		}
	}
}

// Recording must never be required: a nil recorder is the default and every
// call site guards through record/recordEvent, so a turn must complete
// normally with no recorder attached at all.
func TestRun_NilRecorderIsHarmless(t *testing.T) {
	m := &fakeStreamModel{responses: [][]*schema.Message{finalResponse("done")}}
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 0, nil)

	ch, err := agt.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	if len(events) == 0 || events[len(events)-1].Kind != "done" {
		t.Errorf("events = %+v, want the turn to finish with done even with no recorder", events)
	}
}

// WithHistory seeds the agent's history for resume; the seeded messages must
// reach the model ahead of the new turn's user message.
func TestWithHistory_SeedsHistorySentOnFirstRequest(t *testing.T) {
	m := &fakeStreamModel{responses: [][]*schema.Message{finalResponse("done")}}
	seed := []*schema.Message{
		{Role: schema.User, Content: "earlier question"},
		{Role: schema.Assistant, Content: "earlier answer"},
	}
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 0, nil).
		WithHistory(seed)

	ch, err := agt.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	drain(t, ch, func(Event) bool { return true })

	if len(m.seen) == 0 {
		t.Fatal("model was never called")
	}
	sent := m.seen[0]
	if len(sent) != 3 {
		t.Fatalf("sent = %+v, want seeded history (2) plus the new user message (1)", sent)
	}
	if sent[0].Content != "earlier question" || sent[1].Content != "earlier answer" {
		t.Errorf("sent[:2] = %+v, want the seeded history first", sent[:2])
	}
	if sent[2].Content != "continue" {
		t.Errorf("sent[2] = %+v, want the new user message", sent[2])
	}
}

// compact() must report what it did without re-appending the whole rewritten
// history to the recorder -- that history is already fully represented by the
// Append calls already recorded for the messages that went into it.
func TestCompact_RecordsCompactedHistoryEventWithoutReAppending(t *testing.T) {
	rec := &fakeRecorder{}
	agt := New(nil, nil, nil, 1000, &fakeSummarizer{result: "summary"}).WithRecorder(rec)
	agt.history = []*schema.Message{
		{Role: schema.User, Content: "one"},
		{Role: schema.Assistant, Content: "two"},
		{Role: schema.User, Content: "kept"},
	}

	// truncateLen=2: "one" and "two" are summarized away, "kept" survives.
	if err := agt.compact(context.Background(), 2); err != nil {
		t.Fatalf("compact() error = %v", err)
	}

	for _, c := range rec.calls {
		if c.kind == "append" {
			t.Errorf("unexpected Append during compact: %+v", c.msg)
		}
	}

	var found bool
	for _, c := range rec.calls {
		if c.kind == "event" && c.event == "compacted_history" {
			found = true
			if c.detail != "replaced=2 kept=1" {
				t.Errorf("detail = %q, want %q", c.detail, "replaced=2 kept=1")
			}
		}
	}
	if !found {
		t.Error("no compacted_history event recorded")
	}
}

// The forced-compaction path (provider rejected the request as too long) must
// be distinguishable in the recorded transcript from a proactive compaction,
// even though both surface as the same "compacted" Event kind to the UI.
func TestRun_RecordsForcedCompactionEvent(t *testing.T) {
	m := &fakeStreamModel{
		responses: [][]*schema.Message{nil, finalResponse("recovered")},
		errs:      []error{errors.New("context_length_exceeded"), nil},
	}
	rec := &fakeRecorder{}
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 262144, &fakeSummarizer{result: "summary"}).
		WithRecorder(rec)
	agt.history = []*schema.Message{
		{Role: schema.User, Content: "earlier question"},
		{Role: schema.Assistant, Content: "earlier answer"},
	}

	ch, err := agt.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	drain(t, ch, func(Event) bool { return true })

	var found bool
	for _, c := range rec.calls {
		if c.kind == "event" && c.event == "forced_compaction" {
			found = true
			if !strings.Contains(strings.ToLower(c.detail), "provider") {
				t.Errorf("detail = %q, want it to explain the provider forced it", c.detail)
			}
		}
	}
	if !found {
		t.Error("no forced_compaction event recorded")
	}
}

// A cancelled turn must be recorded with no detail. Driving a real
// cancellation through compaction (summarizer returns context.Canceled, as in
// TestRun_CancelDuringCompactionRollsBackHistory) is deterministic, unlike
// racing a context cancel against the fake model's Stream call.
func TestRun_RecordsCancelledEvent(t *testing.T) {
	m := &fakeStreamModel{responses: [][]*schema.Message{}}
	fakeSumm := &fakeSummarizer{err: context.Canceled}
	rec := &fakeRecorder{}

	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 10, fakeSumm).
		WithRecorder(rec)
	agt.history = []*schema.Message{
		{Role: schema.User, Content: strings.Repeat("filler content ", 200)},
	}

	ch, err := agt.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })
	if len(events) != 1 || events[0].Kind != "cancelled" {
		t.Fatalf("events = %+v, want exactly one cancelled event", events)
	}

	var found bool
	for _, c := range rec.calls {
		if c.kind == "event" && c.event == "cancelled" {
			found = true
			if c.detail != "" {
				t.Errorf("detail = %q, want empty", c.detail)
			}
		}
	}
	if !found {
		t.Error("no cancelled event recorded")
	}
}

// A cancelled stream is the only record of a model that will not stop on its
// own -- the exact failure that prompted this change -- so whatever content
// arrived before cancellation must be captured even though history is rolled
// back and never sees it.
func TestRun_CancelDuringStreamRecordsPartialResponse(t *testing.T) {
	m := &fakeStreamModel{
		responses: [][]*schema.Message{nil},
		streamReaders: []*schema.StreamReader[*schema.Message]{
			pipeStream([]*schema.Message{
				{Role: schema.Assistant, Content: "1. README.md 2. app.js "},
			}, context.Canceled),
		},
	}
	rec := &fakeRecorder{}
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 0, nil).
		WithRecorder(rec)

	ch, err := agt.Run(context.Background(), "list files")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	if events[len(events)-1].Kind != "cancelled" {
		t.Fatalf("last event = %+v, want cancelled", events[len(events)-1])
	}

	var detail string
	var found bool
	for _, c := range rec.calls {
		if c.kind == "event" && c.event == "partial_response" {
			found = true
			detail = c.detail
		}
	}
	if !found {
		t.Fatal("no partial_response event recorded")
	}
	if !strings.Contains(detail, "content_len=") {
		t.Errorf("detail = %q, want it to report content_len=", detail)
	}
	if !strings.Contains(detail, "README.md") {
		t.Errorf("detail = %q, want it to contain the streamed text", detail)
	}
}

// No content arrived before cancellation, so there is nothing to excerpt --
// recording an empty partial_response would just be noise in the transcript.
func TestRun_CancelDuringStreamWithNoContentRecordsNoPartialResponse(t *testing.T) {
	m := &fakeStreamModel{
		responses: [][]*schema.Message{nil},
		streamReaders: []*schema.StreamReader[*schema.Message]{
			pipeStream(nil, context.Canceled),
		},
	}
	rec := &fakeRecorder{}
	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 0, nil).
		WithRecorder(rec)

	ch, err := agt.Run(context.Background(), "list files")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	drain(t, ch, func(Event) bool { return true })

	for _, c := range rec.calls {
		if c.kind == "event" && c.event == "partial_response" {
			t.Errorf("unexpected partial_response event with no content: %+v", c)
		}
	}
}

func TestExcerpt_ReturnsShortInputUnchanged(t *testing.T) {
	got := excerpt("short text", 2000)
	if got != "short text" {
		t.Errorf("excerpt() = %q, want unchanged input", got)
	}
}

func TestExcerpt_TruncatesPastLimitAndAppendsByteCount(t *testing.T) {
	s := strings.Repeat("a", 2500)
	got := excerpt(s, 2000)

	if len(got) <= 2000 {
		t.Fatalf("excerpt() length = %d, want it to include the trailing byte-count note", len(got))
	}
	if !strings.HasPrefix(got, strings.Repeat("a", 2000)) {
		t.Error("excerpt() did not preserve the first max bytes")
	}
	if !strings.Contains(got, "+500 bytes") {
		t.Errorf("excerpt() = %q, want it to report the omitted byte count", got)
	}
}

// cancelNoteMarker is the substring every cancellation note carries. Tests
// match on this rather than the full sentence so wording can change freely.
const cancelNoteMarker = "interrupted by the user"

func historyShape(h []*schema.Message) []string {
	var out []string
	for _, m := range h {
		switch {
		case strings.Contains(m.Content, cancelNoteMarker):
			out = append(out, "note")
		case len(m.ToolCalls) > 0:
			out = append(out, "assistant+toolcalls")
		default:
			out = append(out, string(m.Role))
		}
	}
	return out
}

func TestRun_CancelMidStreamKeepsCompletedWork(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{
		responses: [][]*schema.Message{
			toolCallResponse("do_thing", `{"x":"1"}`),
			nil,
		},
		streamReaders: []*schema.StreamReader[*schema.Message]{
			nil,
			pipeStream([]*schema.Message{{Role: schema.Assistant, Content: "partial"}}, context.Canceled),
		},
	}

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 0, nil)

	ch, err := agt.Run(context.Background(), "do the thing")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	if events[len(events)-1].Kind != "cancelled" {
		t.Fatalf("last event = %q, want cancelled (events=%+v)", events[len(events)-1].Kind, events)
	}
	if invoked != 1 {
		t.Fatalf("tool invoked %d times, want 1", invoked)
	}

	// The tool really ran, so its call and result must survive the rollback:
	// a model that cannot see them denies or repeats work already done.
	want := []string{"user", "assistant+toolcalls", "tool", "note"}
	if got := historyShape(agt.history); !slices.Equal(got, want) {
		t.Errorf("history shape = %v, want %v", got, want)
	}
	if n := len(agt.history); n > 0 {
		last := agt.history[n-1].Content
		if !strings.Contains(last, "do_thing") {
			t.Errorf("note = %q, want it to name the completed do_thing call", last)
		}
	}
}

func TestRun_CancelDuringConfirmationDropsTheOrphanedCallButKeepsThePrompt(t *testing.T) {
	invoked := 0
	tl := newRecordingTool(t, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("do_thing", `{"x":"1"}`),
	}}

	agt := New(m, []tool.InvokableTool{tl}, &fakeClassifier{result: safety.Classification{Kind: "write", Risk: safety.Dangerous}}, 0, nil)

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
	if kinds[len(kinds)-1] != "cancelled" {
		t.Errorf("last event kind = %q, want cancelled", kinds[len(kinds)-1])
	}
	// The assistant message is dropped because its ToolCalls never got
	// results and the API rejects that; the user's prompt is not.
	want := []string{"user", "note"}
	if got := historyShape(agt.history); !slices.Equal(got, want) {
		t.Errorf("history shape = %v, want %v", got, want)
	}
}

func TestRun_CancelAfterCompactionStaysInBounds(t *testing.T) {
	m := &fakeStreamModel{
		responses:     [][]*schema.Message{nil},
		streamReaders: []*schema.StreamReader[*schema.Message]{pipeStream(nil, context.Canceled)},
	}
	fakeSumm := &fakeSummarizer{result: "a summary"}

	agt := New(m, nil, &fakeClassifier{result: safety.Classification{Risk: safety.Safe}}, 10, fakeSumm)
	// Three messages pre-compaction collapse to one summary, so the turn
	// start recorded before compaction indexes past the rewritten history.
	for range 3 {
		agt.history = append(agt.history, &schema.Message{
			Role: schema.User, Content: strings.Repeat("filler content ", 100),
		})
	}

	ch, err := agt.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := drain(t, ch, func(Event) bool { return true })

	for _, e := range events {
		if e.Kind == "error" {
			t.Fatalf("got error event after cancelling a compacted turn: %q", e.Payload)
		}
	}
	if events[len(events)-1].Kind != "cancelled" {
		t.Fatalf("last event = %q, want cancelled (events=%+v)", events[len(events)-1].Kind, events)
	}
	want := []string{"system", "user", "note"}
	if got := historyShape(agt.history); !slices.Equal(got, want) {
		t.Errorf("history shape = %v, want %v (summary, the prompt, and the note)", got, want)
	}
}

func TestConsistentPrefix(t *testing.T) {
	user := &schema.Message{Role: schema.User, Content: "go"}
	callsOne := &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "a"}}}
	callsTwo := &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "a"}, {ID: "b"}}}
	result := &schema.Message{Role: schema.Tool, Content: "ok"}
	text := &schema.Message{Role: schema.Assistant, Content: "done"}

	tests := []struct {
		name      string
		history   []*schema.Message
		turnStart int
		want      int
	}{
		{"prompt only", []*schema.Message{user}, 0, 1},
		{"resolved call", []*schema.Message{user, callsOne, result}, 0, 3},
		{"orphaned call dropped", []*schema.Message{user, callsOne}, 0, 1},
		{"partially resolved call dropped", []*schema.Message{user, callsTwo, result}, 0, 1},
		{"fully resolved pair kept", []*schema.Message{user, callsTwo, result, result}, 0, 4},
		{"trailing text kept", []*schema.Message{user, callsOne, result, text}, 0, 4},
		{"second call orphaned", []*schema.Message{user, callsOne, result, callsOne}, 0, 3},
		{"respects turn start", []*schema.Message{text, user, callsOne}, 1, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agt := New(nil, nil, nil, 0, nil)
			agt.history = tt.history
			if got := agt.consistentPrefix(tt.turnStart); got != tt.want {
				t.Errorf("consistentPrefix(%d) = %d, want %d", tt.turnStart, got, tt.want)
			}
		})
	}
}

// The note is synthetic: the user never typed it. Attributing it to the user
// would put words in their mouth, and pickLabel draws a session's label from
// user messages, so a cancelled session could be labelled with this note.
func TestRollbackCancelled_NoteIsASystemMessageNotTheUsers(t *testing.T) {
	agt := New(nil, nil, nil, 0, nil)
	agt.history = []*schema.Message{{Role: schema.User, Content: "go"}}

	agt.rollbackCancelled(0)

	note := agt.history[len(agt.history)-1]
	if !strings.Contains(note.Content, cancelNoteMarker) {
		t.Fatalf("last message = %q, want the cancellation note", note.Content)
	}
	if note.Role != schema.System {
		t.Errorf("note role = %q, want %q", note.Role, schema.System)
	}
}
