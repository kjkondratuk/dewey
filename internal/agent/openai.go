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
)

// compactionThreshold is the fraction of the context window that triggers
// automatic compaction.
const compactionThreshold = 0.90

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
					// drop the user turn too, as if the question was never asked
					a.history = a.history[:len(a.history)-1]
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
					a.history = append(a.history, schema.ToolMessage(
						"error: unknown tool "+tc.Function.Name, tc.ID))
					continue
				}

				classification := a.classify(ctx, tc.Function.Name, tc.Function.Arguments)
				if classification.Risk != safety.Safe {
					allowed, cancelled := a.awaitConfirmation(ctx, ch, tc.Function.Name, classification, tc.Function.Arguments)
					if cancelled {
						a.history = a.history[:truncateLen]
						ch <- Event{Kind: "cancelled"}
						return
					}
					if !allowed {
						a.history = append(a.history, schema.ToolMessage(
							"error: user denied permission for this action", tc.ID))
						continue
					}
				}

				ch <- Event{Kind: "tool", Payload: formatToolInvocation(tc.Function.Name, tc.Function.Arguments)}
				result, err := t.InvokableRun(ctx, tc.Function.Arguments)
				if err != nil {
					result = "error: " + err.Error()
				}
				a.history = append(a.history, schema.ToolMessage(result, tc.ID))
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

func formatToolInvocation(name, argsJSON string) string {
	return fmt.Sprintf("Running %s:\n%s", name, prettyPrintArgs(argsJSON))
}

func formatConfirmMessage(name string, c safety.Classification, argsJSON string) string {
	kind := ""
	if c.Kind != "" {
		kind = " (" + strings.ToUpper(c.Kind) + ")"
	}

	return fmt.Sprintf("%s wants to run%s [risk: %s]\n%s\nAllow? [y/n]", name, kind, c.Risk, prettyPrintArgs(argsJSON))
}
