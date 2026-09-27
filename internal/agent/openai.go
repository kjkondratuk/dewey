// internal/agent/openai.go
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

type openaiAgent struct {
	cm         model.ToolCallingChatModel
	tools      map[string]tool.InvokableTool
	descs      map[string]string
	classifier safety.Classifier
	overrides  safety.RiskOverrides
	history    []*schema.Message

	contextWindow int
	summarizer    compaction.Summarizer

	// lastKnownTokens/lastKnownAt track the most recent provider-reported
	// total token count and the history length at which it was recorded,
	// so estimateTokens only needs to estimate the delta since then.
	lastKnownTokens int
	lastKnownAt     int
}

// New builds an agent backed by cm, which must already have the tool schemas
// bound (via cm.WithTools). tools supplies the matching executable
// implementations, keyed by name, so tool calls the model requests can
// actually be run. classifier assigns a risk tier to each resolved tool
// call; overrides pins specific tool names to a fixed tier, bypassing the
// classifier for that tool. contextWindow is the model's context size in
// tokens (0 disables automatic compaction); summarizer condenses history
// when compaction triggers.
func New(
	cm model.ToolCallingChatModel,
	tools []tool.InvokableTool,
	classifier safety.Classifier,
	overrides safety.RiskOverrides,
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
		overrides:     overrides,
		contextWindow: contextWindow,
		summarizer:    summarizer,
	}
}

func (a *openaiAgent) Run(ctx context.Context, input string) (<-chan Event, error) {
	truncateLen := len(a.history)
	a.history = append(a.history, &schema.Message{
		Role:    schema.User,
		Content: input,
	})

	ch := make(chan Event)
	go func() {
		defer close(ch)

		// return an error if the loop panics
		defer func() {
			if r := recover(); r != nil {
				ch <- Event{Kind: "error", Payload: fmt.Sprintf("panic: %v", r)}
			}
		}()

		if estimated := a.estimateTokens(); truncateLen > 0 && a.shouldCompact(estimated) {
			if err := a.compact(ctx, truncateLen); err != nil {
				if errors.Is(err, context.Canceled) {
					a.history = a.history[:truncateLen]
					ch <- Event{Kind: "cancelled"}
					return
				}
				ch <- Event{Kind: "error", Payload: "compaction failed: " + err.Error()}
				return
			}
			ch <- Event{Kind: "compacted", Payload: fmt.Sprintf(
				"Compacted conversation history (was ~%d/%d tokens, %.0f%%).",
				estimated, a.contextWindow, 100*float64(estimated)/float64(a.contextWindow))}
		}

		// Each iteration is one full model turn: stream a response, then run
		// any tool calls it asked for. Loops back around with the tool
		// results appended to history until the model stops calling tools.
		for {
			sr, err := a.cm.Stream(ctx, a.history)
			if err != nil {
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
					// Roll the whole turn back, as if the question was never
					// asked. Dropping just the last message is only equivalent
					// on the first pass: cancelling during a later model turn
					// would strip one tool result and leave an assistant
					// message with orphaned ToolCalls, which the API rejects.
					a.history = a.history[:truncateLen]
					ch <- Event{Kind: "cancelled"}
					return
				}
				if err != nil {
					sr.Close()
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
				ch <- Event{Kind: "error", Payload: err.Error()}
				return
			}
			a.history = append(a.history, full)
			a.recordUsage(full)

			if len(full.ToolCalls) == 0 {
				ch <- Event{Kind: "done"}
				return
			}

			// resolve all tool calls
			for _, tc := range full.ToolCalls {
				t, ok := a.tools[tc.Function.Name]
				if !ok {
					a.history = append(a.history, toolResult(
						"error: unknown tool "+tc.Function.Name, tc.ID, tc.Function.Name))
					continue
				}

				// confirmed records whether the user was just shown this exact
				// call and approved it, so the status line below doesn't repeat
				// a command that's already on screen.
				confirmed := false
				classification := a.classify(ctx, tc.Function.Name, tc.Function.Arguments)
				if classification.Risk != safety.Safe {
					allowed, cancelled := a.awaitConfirmation(ctx, ch, tc.Function.Name, classification, tc.Function.Arguments)
					if cancelled {
						a.history = a.history[:truncateLen]
						ch <- Event{Kind: "cancelled"}
						return
					}
					if !allowed {
						a.history = append(a.history, toolResult(deniedToolResult, tc.ID, tc.Function.Name))
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
				a.history = append(a.history, toolResult(result, tc.ID, tc.Function.Name))
			}
			// loop back around and stream again with tool results in history
		}
	}()

	return ch, nil
}

// classify resolves the risk classification for a tool call: a config
// override always wins and skips the classifier; otherwise the classifier
// is consulted, which itself fails safe to Dangerous/write on any error.
func (a *openaiAgent) classify(ctx context.Context, name, argsJSON string) safety.Classification {
	if tier, ok := a.overrides[name]; ok {
		return safety.Classification{Risk: tier}
	}
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
// recorded.
func (a *openaiAgent) estimateTokens() int {
	if a.lastKnownAt > 0 && a.lastKnownAt <= len(a.history) {
		return a.lastKnownTokens + compaction.EstimateTokens(a.history[a.lastKnownAt:])
	}
	return compaction.EstimateTokens(a.history)
}

// shouldCompact reports whether estimated tokens have crossed
// compactionThreshold of the context window. Compaction is disabled
// entirely when contextWindow <= 0.
func (a *openaiAgent) shouldCompact(estimated int) bool {
	return a.contextWindow > 0 && estimated >= int(float64(a.contextWindow)*compactionThreshold)
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
	return nil
}

func prettyPrintArgs(argsJSON string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(argsJSON), "", "  "); err == nil {
		return buf.String()
	}
	return argsJSON
}

// describeArgs renders tool arguments for a human reader. A lone string
// argument — the shape of run_shell_command's {"command": "..."} — is returned
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
