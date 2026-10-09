// internal/agent/openai.go
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/kjkondratuk/dewey/internal/compaction"
	"github.com/kjkondratuk/dewey/internal/safety"
	"github.com/kjkondratuk/dewey/prompts"
)

// compactionThreshold is the fraction of the context window that triggers
// automatic compaction.
const compactionThreshold = 0.90

// deniedToolResult is what the model sees when the user declines a tool call.
// It deliberately avoids the word "error" and states outright that the
// capability exists, because the failure mode being guarded against is the
// model reading a denial as a missing capability and reporting that it is
// unable to perform the action at all. The text lives in
// prompts/tool_denied.md.
var deniedToolResult = prompts.ToolDenied

// toolResult builds a tool-result message with the originating tool's name
// attached. Both name fields are set on purpose: eino's WithToolName only
// populates Message.ToolName, but the OpenAI adapter serializes Message.Name,
// so setting either one alone silently drops the attribution on the wire.
func toolResult(content, toolCallID, toolName string) *schema.Message {
	msg := schema.ToolMessage(content, toolCallID, schema.WithToolName(toolName))
	msg.Name = toolName
	return msg
}

// excerpt bounds model-authored text for a diagnostic record. A runaway
// generation is precisely the case being recorded, so the whole thing must
// not be written: the point is to see what it was repeating, which the first
// few hundred characters show as well as a megabyte would.
func excerpt(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("... [+%d bytes]", len(s)-max)
}

// SystemPromptProvider supplies the system message for each model call. It is
// consulted per Stream rather than once per session so a memory saved mid-turn
// takes effect immediately.
type SystemPromptProvider interface {
	System(ctx context.Context) (*schema.Message, error)
}

// noticeReporter is an optional capability of a SystemPromptProvider: anything
// the user should be told about the state of their profile. Optional so the
// core interface stays a single method.
type noticeReporter interface {
	Notices() []string
}

// Recorder persists the conversation as it happens. Narrow on purpose: the
// agent reports what it appended and what happened, and knows nothing about
// where it goes.
type Recorder interface {
	Append(msg *schema.Message)
	Event(event, detail string)
}

type openaiAgent struct {
	cm         model.ToolCallingChatModel
	tools      map[string]tool.InvokableTool
	descs      map[string]string
	classifier safety.Classifier
	history    []*schema.Message

	// systemProvider is deliberately not part of history: compact() rewrites
	// history[:truncateLen], so a system message stored there would be
	// summarized away the first time compaction fired.
	systemProvider SystemPromptProvider

	contextWindow int
	summarizer    compaction.Summarizer

	// lastKnownTokens/lastKnownAt track the most recent provider-reported
	// total token count and the history length at which it was recorded,
	// so estimateTokens only needs to estimate the delta since then.
	lastKnownTokens int
	lastKnownAt     int

	// lastNotices is the set already shown, so a steady-state problem is
	// reported once rather than before every model call.
	lastNotices []string

	// refreshWindow re-resolves the context window once the provider has
	// served a request; windowRefreshed makes that once per session.
	refreshWindow   ContextWindowRefresher
	windowRefreshed bool

	// windowFromRejection records that contextWindow was learned from a
	// provider rejecting a request as too long, which outranks anything
	// discovery can infer afterwards.
	windowFromRejection bool

	// recorder persists the conversation as it happens, for debugging and
	// resume. Nil means recording is disabled for this run.
	recorder Recorder
}

// New builds an agent backed by cm, which must already have the tool schemas
// bound (via cm.WithTools). tools supplies the matching executable
// implementations, keyed by name, so tool calls the model requests can
// actually be run. classifier assigns a risk tier to each resolved tool
// call, applying a deterministic floor the model cannot lower. contextWindow
// is the model's context size in
// tokens (0 disables automatic compaction); summarizer condenses history
// when compaction triggers.
func New(
	cm model.ToolCallingChatModel,
	tools []tool.InvokableTool,
	classifier safety.Classifier,
	contextWindow int,
	summarizer compaction.Summarizer,
) *openaiAgent {
	byName := make(map[string]tool.InvokableTool, len(tools))
	descs := make(map[string]string, len(tools))
	for _, t := range tools {
		info, err := t.Info(context.Background())
		if err != nil {
			continue
		}
		byName[info.Name] = t
		descs[info.Name] = info.Desc
	}
	return &openaiAgent{
		cm:            cm,
		tools:         byName,
		descs:         descs,
		classifier:    classifier,
		contextWindow: contextWindow,
		summarizer:    summarizer,
	}
}

// ContextWindowRefresher re-resolves the model's context window. The bool
// reports whether the value is a trustworthy measurement; false means
// "nothing better than what you already have". It is a function rather than
// an interface so the agent stays independent of how the window is
// discovered — see config.RefreshContextWindow for the LM Studio
// implementation.
type ContextWindowRefresher func(ctx context.Context) (int, bool)

// WithContextWindowRefresher attaches a refresher consulted once per session,
// after the first successfully served request. Separate from New so existing
// call sites and their tests stay unchanged.
func (a *openaiAgent) WithContextWindowRefresher(f ContextWindowRefresher) *openaiAgent {
	a.refreshWindow = f
	return a
}

// WithSystemPrompt attaches a system prompt provider. Separate from New so the
// existing call sites and their tests stay unchanged.
func (a *openaiAgent) WithSystemPrompt(p SystemPromptProvider) *openaiAgent {
	a.systemProvider = p
	return a
}

// WithRecorder attaches a Recorder that persists the conversation as it
// happens. A nil recorder (the zero value, and also what passing nil
// explicitly leaves in place) disables recording entirely; every call site
// guards through record/recordEvent so that is always safe.
func (a *openaiAgent) WithRecorder(r Recorder) *openaiAgent {
	a.recorder = r
	return a
}

// WithHistory seeds the agent's conversation history, for resuming a session
// recorded by a Recorder. The seeded messages are sent on the very first
// request, ahead of anything Run appends.
func (a *openaiAgent) WithHistory(msgs []*schema.Message) *openaiAgent {
	a.history = msgs
	return a
}

// record appends msg to the recorder, if one is attached. A small helper so
// every append site reads the same way rather than repeating a nil check.
func (a *openaiAgent) record(msg *schema.Message) {
	if a.recorder != nil {
		a.recorder.Append(msg)
	}
}

// recordEvent reports an out-of-band occurrence to the recorder, if one is
// attached.
func (a *openaiAgent) recordEvent(event, detail string) {
	if a.recorder != nil {
		a.recorder.Event(event, detail)
	}
}

// messages returns what to send: the system message, if any, followed by the
// conversation. The system message is rebuilt per call.
func (a *openaiAgent) messages(ctx context.Context) ([]*schema.Message, error) {
	if a.systemProvider == nil {
		return a.history, nil
	}
	sys, err := a.systemProvider.System(ctx)
	if err != nil {
		return nil, err
	}
	if sys == nil {
		return a.history, nil
	}
	return append([]*schema.Message{sys}, a.history...), nil
}

func (a *openaiAgent) Run(ctx context.Context, input string) (<-chan Event, error) {
	turnStart := len(a.history)
	userMsg := &schema.Message{
		Role:    schema.User,
		Content: input,
	}
	a.history = append(a.history, userMsg)
	a.record(userMsg)

	ch := make(chan Event)
	go func() {
		defer close(ch)

		// return an error if the loop panics
		defer func() {
			if r := recover(); r != nil {
				payload := fmt.Sprintf("panic: %v", r)
				a.recordEvent("error", payload)
				ch <- Event{Kind: "error", Payload: payload}
			}
		}()

		if estimated := a.estimateTokens(); turnStart > 0 && a.shouldCompact(estimated) {
			if err := a.compact(ctx, turnStart); err != nil {
				if errors.Is(err, context.Canceled) {
					a.rollbackCancelled(turnStart)
					a.recordEvent("cancelled", "")
					ch <- Event{Kind: "cancelled"}
					return
				}
				payload := "compaction failed: " + err.Error()
				a.recordEvent("error", payload)
				ch <- Event{Kind: "error", Payload: payload}
				return
			}
			compactedPayload := fmt.Sprintf(
				"Compacted conversation history (was ~%d/%d tokens, %.0f%%).",
				estimated, a.contextWindow, 100*float64(estimated)/float64(a.contextWindow))
			a.recordEvent("compacted", compactedPayload)
			ch <- Event{Kind: "compacted", Payload: compactedPayload}
			// compact replaced everything before this turn with a single
			// summary message, so the turn now starts just after it.
			turnStart = 1
		}

		// forcedCompaction limits the provider-overflow backstop below to one
		// attempt per turn, so a server that rejects every request fails
		// rather than compacting in a loop.
		forcedCompaction := false

		// Each iteration is one full model turn: stream a response, then run
		// any tool calls it asked for. Loops back around with the tool
		// results appended to history until the model stops calling tools.
		for {
			outbound, err := a.messages(ctx)
			if err != nil {
				payload := "system prompt unavailable: " + err.Error()
				a.recordEvent("error", payload)
				ch <- Event{Kind: "error", Payload: payload}
				return
			}
			// Report profile problems that appeared since the last call. The
			// startup report only covers the first load, but saving a memory
			// can evict another for budget -- precisely when the user has just
			// been told "Saved."
			if notice := a.newNotices(); notice != "" {
				a.recordEvent("notice", notice)
				ch <- Event{Kind: "notice", Payload: notice}
			}

			a.recordEvent("request", fmt.Sprintf(
				"messages=%d estimated_tokens=%d window=%d", len(outbound), a.estimateTokens(), a.contextWindow))
			sr, err := a.cm.Stream(ctx, outbound)
			if err != nil {
				// Backstop for a believed context window larger than the one
				// the provider actually serves — see DiscoverContextWindow,
				// which can only infer the window from a model's advertised
				// architectural maximum when that model is not yet loaded. In
				// that state shouldCompact never fires, so the rejection is
				// the only signal that history has to shrink.
				//
				// Only the request-time error is handled: an oversized prompt
				// is rejected before generation starts. An overflow surfacing
				// mid-stream, after content has been emitted, is about output
				// length and compaction would not help.
				if !forcedCompaction && turnStart > 0 && isContextOverflow(err) {
					forcedCompaction = true

					evt, cerr := a.forceCompact(ctx, turnStart)
					if cerr != nil {
						if errors.Is(cerr, context.Canceled) {
							a.rollbackCancelled(turnStart)
							a.recordEvent("cancelled", "")
							ch <- Event{Kind: "cancelled"}
							return
						}
						payload := "compaction after a context overflow failed: " + cerr.Error()
						a.recordEvent("error", payload)
						ch <- Event{Kind: "error", Payload: payload}
						return
					}

					// As above: history was rewritten, so the start moves.
					turnStart = 1
					a.recordEvent("forced_compaction", evt.Payload)
					ch <- evt
					continue
				}

				a.recordEvent("error", err.Error())
				ch <- Event{Kind: "error", Payload: err.Error()}
				return
			}

			var chunks []*schema.Message
			for {
				msg, err := sr.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if errors.Is(err, context.Canceled) {
					sr.Close()
					// A cancelled stream is the only record we get of a model
					// that will not stop on its own, which is exactly the
					// failure worth diagnosing. History is rolled back below,
					// so this is recorded as an event rather than a message —
					// it never became part of the conversation.
					if partial, cErr := schema.ConcatMessages(chunks); cErr == nil && partial.Content != "" {
						a.recordEvent("partial_response", fmt.Sprintf(
							"content_len=%d excerpt=%s", len(partial.Content), excerpt(partial.Content, 2000)))
					}
					// Nothing of this response was appended, so the
					// rollback keeps the whole turn up to here -- including
					// any tool calls that already ran. See consistentPrefix.
					a.rollbackCancelled(turnStart)
					a.recordEvent("cancelled", "")
					ch <- Event{Kind: "cancelled"}
					return
				}
				if err != nil {
					sr.Close()
					a.recordEvent("error", err.Error())
					ch <- Event{Kind: "error", Payload: err.Error()}
					return
				}
				chunks = append(chunks, msg)
				if msg.Content != "" {
					ch <- Event{Kind: "chunk", Payload: msg.Content}
				}
			}
			sr.Close()

			full, err := schema.ConcatMessages(chunks)
			if err != nil {
				a.recordEvent("error", err.Error())
				ch <- Event{Kind: "error", Payload: err.Error()}
				return
			}
			a.history = append(a.history, full)
			a.record(full)
			a.recordUsage(full)

			// The provider just served a request, so the model is loaded and
			// its real window is discoverable for the first time.
			if notice := a.refreshContextWindow(ctx); notice != "" {
				a.recordEvent("notice", notice)
				ch <- Event{Kind: "notice", Payload: notice}
			}

			if len(full.ToolCalls) == 0 {
				ch <- Event{Kind: "done"}
				return
			}

			// resolve all tool calls
			for _, tc := range full.ToolCalls {
				t, ok := a.tools[tc.Function.Name]
				if !ok {
					unknownResult := toolResult(
						"error: unknown tool "+tc.Function.Name, tc.ID, tc.Function.Name)
					a.history = append(a.history, unknownResult)
					a.record(unknownResult)
					continue
				}

				// confirmed records whether the user was just shown this exact
				// call and approved it, so the status line below doesn't repeat
				// a command that's already on screen.
				confirmed := false
				classification := a.classify(ctx, tc.Function.Name, tc.Function.Arguments)
				if classification.RequiresApproval() {
					allowed, cancelled := a.awaitConfirmation(ctx, ch, tc.Function.Name, classification, tc.Function.Arguments)
					if cancelled {
						a.rollbackCancelled(turnStart)
						a.recordEvent("cancelled", "")
						ch <- Event{Kind: "cancelled"}
						return
					}
					if !allowed {
						deniedResult := toolResult(deniedToolResult, tc.ID, tc.Function.Name)
						a.history = append(a.history, deniedResult)
						a.record(deniedResult)
						continue
					}
					confirmed = true
				}

				if confirmed {
					ch <- Event{Kind: "tool", Payload: formatToolInvocationBrief()}
				} else {
					ch <- Event{Kind: "tool", Payload: formatToolInvocation(tc.Function.Name, tc.Function.Arguments)}
				}

				result, err := t.InvokableRun(ctx, tc.Function.Arguments)
				if err != nil {
					result = "error: " + err.Error()
				}
				resultMsg := toolResult(result, tc.ID, tc.Function.Name)
				a.history = append(a.history, resultMsg)
				a.record(resultMsg)
			}
			// loop back around and stream again with tool results in history
		}
	}()

	return ch, nil
}

// newNotices returns any notices not yet shown, joined, or "" if nothing is
// new. Must be called after messages(), which is what triggers the reload the
// notices describe.
func (a *openaiAgent) newNotices() string {
	reporter, ok := a.systemProvider.(noticeReporter)
	if !ok {
		return ""
	}

	current := reporter.Notices()
	seen := make(map[string]bool, len(a.lastNotices))
	for _, n := range a.lastNotices {
		seen[n] = true
	}

	var fresh []string
	for _, n := range current {
		if !seen[n] {
			fresh = append(fresh, n)
		}
	}
	a.lastNotices = current

	if len(fresh) == 0 {
		return ""
	}
	return strings.Join(fresh, "\n")
}

// classify resolves the classification for a tool call. The classifier fails
// safe to Dangerous/write on any error and applies the deterministic shell
// floor itself, so the result is usable without special-casing err.
func (a *openaiAgent) classify(ctx context.Context, name, argsJSON string) safety.Classification {
	classification, _ := a.classifier.Classify(ctx, name, a.descs[name], argsJSON)
	return classification
}

// awaitConfirmation emits a "confirm" event and blocks until the caller
// responds on the event's Confirm channel, or ctx is cancelled.
func (a *openaiAgent) awaitConfirmation(ctx context.Context, ch chan<- Event, name string, c safety.Classification, argsJSON string) (allowed, cancelled bool) {
	resp := make(chan bool, 1)
	ch <- Event{Kind: "confirm", Payload: formatConfirmMessage(name, c, argsJSON), Confirm: resp}

	select {
	case allowed = <-resp:
		return allowed, false
	case <-ctx.Done():
		return false, true
	}
}

// recordUsage stores the provider-reported token total for full (if any) as
// a trusted baseline for future estimateTokens calls.
func (a *openaiAgent) recordUsage(full *schema.Message) {
	if full.ResponseMeta == nil || full.ResponseMeta.Usage == nil {
		return
	}
	if total := full.ResponseMeta.Usage.TotalTokens; total > 0 {
		a.lastKnownTokens = total
		a.lastKnownAt = len(a.history)
	}
}

// estimateTokens returns the current context usage: the last provider-
// reported total plus a tokenizer-based estimate of everything appended
// since, or a full tokenizer estimate if no provider total was ever
// recorded. The system message is added on top in either case — it is sent
// with every request but lives outside history, so neither the baseline nor
// the delta covers it.
func (a *openaiAgent) estimateTokens() int {
	total := 0
	if a.systemProvider != nil {
		if sys, err := a.systemProvider.System(context.Background()); err == nil && sys != nil {
			total += compaction.EstimateTokens([]*schema.Message{sys})
		}
	}
	if a.lastKnownAt > 0 && a.lastKnownAt <= len(a.history) {
		return total + a.lastKnownTokens + compaction.EstimateTokens(a.history[a.lastKnownAt:])
	}
	return total + compaction.EstimateTokens(a.history)
}

// refreshContextWindow re-resolves the context window once per session, after
// the provider has successfully served a request. At startup the model is
// often not resident, so DiscoverContextWindow can do no better than clamp
// the architectural ceiling; a served request proves the model is loaded and
// the real window is finally observable. Without this, launching dewey
// against an idle server means running at the clamp for the whole session.
//
// A window already learned from a provider rejection is left alone: that
// rejection is direct evidence about what the server will serve, discovery is
// an inference, and raising the window back would walk into the same overflow
// again.
//
// Returns a notice to show the user, or "" when nothing changed.
func (a *openaiAgent) refreshContextWindow(ctx context.Context) string {
	if a.refreshWindow == nil || a.windowRefreshed {
		return ""
	}
	a.windowRefreshed = true

	if a.windowFromRejection {
		return ""
	}

	window, ok := a.refreshWindow(ctx)
	if !ok || window == a.contextWindow {
		return ""
	}

	previous := a.contextWindow
	a.contextWindow = window
	return fmt.Sprintf(
		"Context window updated from ~%d to %d tokens now that the model is loaded.",
		previous, window)
}

// shouldCompact reports whether estimated tokens have crossed
// compactionThreshold of the context window. Compaction is disabled
// entirely when contextWindow <= 0.
func (a *openaiAgent) shouldCompact(estimated int) bool {
	return a.contextWindow > 0 && estimated >= int(float64(a.contextWindow)*compactionThreshold)
}

// consistentPrefix returns the largest prefix of a.history, at or after
// turnStart, in which every assistant message's ToolCalls have a matching
// tool result. It is the point a cancelled turn is trimmed back to.
//
// Cutting all the way to turnStart would be simpler, but the tool calls that
// already ran had real effects -- files written, commits made -- and a model
// that cannot see them in history denies that work or repeats it. Keeping any
// more than this would leave an assistant message with orphaned ToolCalls,
// which the API rejects.
func (a *openaiAgent) consistentPrefix(turnStart int) int {
	boundary := turnStart
	for i := turnStart; i < len(a.history); {
		if calls := len(a.history[i].ToolCalls); calls > 0 {
			results, j := 0, i+1
			for j < len(a.history) && a.history[j].Role == schema.Tool {
				results++
				j++
			}
			if results < calls {
				break
			}
			i = j
		} else {
			i++
		}
		boundary = i
	}
	return boundary
}

// rollbackCancelled trims a cancelled turn to the last valid boundary and
// appends a note describing the interruption. The note carries as much weight
// as the trim: without it history ends on a user message with no reply, which
// reads to the model as a question it declined to answer.
//
// The note is a system message, matching the summary compact() inserts. The
// user role would be read more reliably by a small model, but it would also
// attribute words to the user that the user never typed -- and pickLabel
// draws a session's label from user messages, so a cancelled session could
// end up labelled with this note.
func (a *openaiAgent) rollbackCancelled(turnStart int) {
	// A compaction earlier in this turn rewrote history and left turnStart
	// indexing past its end.
	turnStart = min(turnStart, len(a.history))
	a.history = a.history[:a.consistentPrefix(turnStart)]

	note := &schema.Message{Role: schema.System, Content: cancelNote(a.ranToolNames(turnStart))}
	a.history = append(a.history, note)
	a.record(note)
}

// ranToolNames lists the distinct tools that returned a result in
// a.history[from:], in call order, for the cancellation note.
func (a *openaiAgent) ranToolNames(from int) []string {
	var names []string
	for _, msg := range a.history[from:] {
		if msg.Role != schema.Tool || msg.ToolName == "" {
			continue
		}
		if !slices.Contains(names, msg.ToolName) {
			names = append(names, msg.ToolName)
		}
	}
	return names
}

// cancelNote tells the model its turn was cut short and what outlived it, so
// it checks the state of the work rather than assuming it either finished or
// never began.
func cancelNote(ran []string) string {
	if len(ran) == 0 {
		return "[The previous turn was interrupted by the user before any tool ran. " +
			"Nothing was changed.]"
	}
	return fmt.Sprintf(
		"[The previous turn was interrupted by the user. These tools had already run and "+
			"their effects persist: %s. Check the current state of that work before redoing it.]",
		strings.Join(ran, ", "))
}

// compact summarizes a.history[:truncateLen] (everything before the
// current turn's just-appended user message) and replaces it with a single
// summary message, leaving the current user message untouched.
func (a *openaiAgent) compact(ctx context.Context, truncateLen int) error {
	if truncateLen == 0 {
		return nil
	}

	summary, err := a.summarizer.Summarize(ctx, a.history[:truncateLen])
	if err != nil {
		return err
	}

	summaryMsg := &schema.Message{
		Role:    schema.System,
		Content: "Summary of the conversation so far:\n" + summary,
	}
	a.history = append([]*schema.Message{summaryMsg}, a.history[truncateLen:]...)
	a.lastKnownTokens = 0
	a.lastKnownAt = 0
	// The rewritten history is not re-appended to the recorder -- it already
	// has every message that went into the summary, plus everything kept.
	// Re-appending would duplicate the whole kept tail in the transcript.
	a.recordEvent("compacted_history", fmt.Sprintf("replaced=%d kept=%d", truncateLen, len(a.history)-1))
	return nil
}

// contextOverflowMarkers are substrings that identify a provider rejecting a
// request because the prompt exceeds its context window. There is no standard
// error code for this across OpenAI-compatible servers, so matching is by
// message text, lowercased. Add markers as new providers turn up; a missed
// marker costs the backstop, not correctness.
var contextOverflowMarkers = []string{
	"context_length_exceeded",
	"maximum context length",
	"context window exceeded",
	"reduce the length of the messages",
	"overflow policy",
	"prompt is too long",
}

// isContextOverflow reports whether err is a provider complaining that the
// prompt is too long for its context window, as opposed to any other failure.
// It is deliberately narrow: treating an unrelated error as an overflow would
// summarize away real history to no purpose.
func isContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range contextOverflowMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// minForcedContextWindow floors the window learned from a provider rejection.
// The rejection bounds the real limit from above, but a.estimateTokens() can
// undercount badly when the provider's tokenizer differs from tiktoken, and
// adopting a tiny window verbatim would compact on every subsequent turn.
const minForcedContextWindow = 4096

// forceCompact compacts because the provider rejected the request as too long,
// rather than because the local estimate crossed a threshold. It ignores
// contextWindow on the way in — the provider has already ruled — and on the
// way out lowers the believed window, since the rejection proves the current
// value is an over-estimate. Without that, discovery reporting a window larger
// than the server will serve means shouldCompact never fires and every turn
// has to learn the same lesson again.
func (a *openaiAgent) forceCompact(ctx context.Context, truncateLen int) (Event, error) {
	estimated := a.estimateTokens()

	if err := a.compact(ctx, truncateLen); err != nil {
		return Event{}, err
	}

	if learned := max(minForcedContextWindow, estimated); a.contextWindow <= 0 || learned < a.contextWindow {
		a.contextWindow = learned
		a.windowFromRejection = true
	}

	return Event{Kind: "compacted", Payload: fmt.Sprintf(
		"The provider rejected the request as too long for its context window. "+
			"Compacted and retrying; assuming ~%d tokens from here.", a.contextWindow)}, nil
}

func prettyPrintArgs(argsJSON string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(argsJSON), "", "  "); err == nil {
		return buf.String()
	}
	return argsJSON
}

// describeArgs renders tool arguments for a human reader. A lone string
// argument — the shape of Bash's {"command": "..."} — is returned
// bare, so a command reads as a command, with real newlines instead of
// JSON-escaped ones. Anything else falls back to indented JSON rather than
// risk silently dropping fields.
func describeArgs(argsJSON string) string {
	var fields map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &fields); err == nil && len(fields) == 1 {
		for _, v := range fields {
			if s, ok := v.(string); ok {
				return strings.TrimRight(s, "\n")
			}
		}
	}
	return prettyPrintArgs(argsJSON)
}

// fence wraps body in a code fence long enough that no backtick run inside it
// can terminate the block early — tool arguments are arbitrary text, and a
// command containing a fence would otherwise break out into the surrounding
// markdown.
func fence(body string) string {
	longest, run := 0, 0
	for _, r := range body {
		if r != '`' {
			run = 0
			continue
		}
		run++
		if run > longest {
			longest = run
		}
	}
	delim := strings.Repeat("`", max(3, longest+1))
	return delim + "\n" + body + "\n" + delim
}

// plainLine flattens model-authored text into a single line that can't take
// over the markdown around it. The classifier's summary is untrusted output
// rendered inside a security prompt, so it should describe the command, not
// restyle the prompt asking about it.
func plainLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	// Collapsing to one line already removes every interior line start, so
	// block constructs (headings, lists, quotes, fences) can only trigger
	// from the very first character. Escape it when it's punctuation —
	// CommonMark only permits backslash-escaping ASCII punctuation, so doing
	// this unconditionally would leave a literal backslash before a letter.
	if isASCIIPunct(s[0]) {
		return `\` + s
	}
	return s
}

func isASCIIPunct(b byte) bool {
	return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", b) >= 0
}

func formatToolInvocation(name, argsJSON string) string {
	return fmt.Sprintf("Running %s:\n%s", name, fence(describeArgs(argsJSON)))
}

// formatToolInvocationBrief is used when the user just approved this exact
// call. The command is already on screen immediately above, so repeating it
// only adds noise.
func formatToolInvocationBrief() string {
	return "Running…"
}

func formatConfirmMessage(name string, c safety.Classification, argsJSON string) string {
	kind := ""
	if c.Kind != "" {
		kind = " (" + strings.ToUpper(c.Kind) + ")"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s wants to run%s [risk: %s]\n", name, kind, c.Risk)
	if summary := plainLine(c.Summary); summary != "" {
		fmt.Fprintf(&b, "\n%s\n", summary)
	}
	b.WriteString("\n")
	b.WriteString(fence(describeArgs(argsJSON)))
	b.WriteString("\nAllow? [y/n]")
	return b.String()
}
