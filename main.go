package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/cloudwego/eino/schema"
	"github.com/kjkondratuk/dewey/internal/agent"
	"github.com/kjkondratuk/dewey/internal/app"
	"github.com/kjkondratuk/dewey/internal/compaction"
	"github.com/kjkondratuk/dewey/internal/config"
	"github.com/kjkondratuk/dewey/internal/memory"
	"github.com/kjkondratuk/dewey/internal/safety"
	tools2 "github.com/kjkondratuk/dewey/internal/tools"
	"github.com/kjkondratuk/dewey/prompts"
)

func main() {
	ctx := context.Background()

	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("failed to determine working directory: %v", err)
	}

	cfg, ruleNotices, err := config.Load(cwd)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	intermediateModel, err := config.BuildChatModel(ctx, cfg.Intermediate)
	if err != nil {
		log.Fatalf("failed to create intermediate model: %v", err)
	}

	simpleModel, err := config.BuildChatModel(ctx, cfg.Simple)
	if err != nil {
		log.Fatalf("failed to create simple model: %v", err)
	}
	classifier := safety.NewLLMClassifier(simpleModel, cfg.ShellRules)
	summarizer := compaction.NewLLMSummarizer(simpleModel)
	contextWindow := config.DiscoverContextWindow(ctx, cfg.Intermediate)

	systemPrompt, toolRecorder, provider, notices := buildMemory(cwd, cfg)
	notices = append(ruleNotices, notices...)

	// Force one load here so an unreadable memory directory or a dropped
	// memory is reported at startup, rather than surfacing mid-turn as a
	// failed model call.
	if provider != nil {
		if _, err := provider.Block(ctx); err != nil {
			notices = append(notices, "memory unavailable: "+err.Error())
		} else {
			notices = append(notices, provider.Notices()...)
		}
	}

	// register tools
	tools := tools2.NewToolFactory(toolRecorder).DefaultInvokableToolset()
	infos := make([]*schema.ToolInfo, len(tools))
	for i, t := range tools {
		var err error
		infos[i], err = t.Info(ctx)
		if err != nil {
			log.Fatalf("failed to get tool info: %v", err)
		}
	}
	tm, err := intermediateModel.WithTools(infos)
	if err != nil {
		log.Fatalf("failed to bind tools: %v", err)
	}

	agt := agent.New(tm, tools, classifier, contextWindow, summarizer).
		WithSystemPrompt(systemPrompt).
		// Startup discovery runs before any request, so the model is often
		// not loaded yet and contextWindow above may be a clamped guess. Once
		// the provider serves a request the real window is observable.
		WithContextWindowRefresher(func(ctx context.Context) (int, bool) {
			return config.RefreshContextWindow(ctx, cfg.Intermediate)
		})

	prog := tea.NewProgram(initModel(agt, notices))
	_, err = prog.Run()
	if err != nil {
		log.Fatalf("program exited with error: %v", err)
	}
}

// buildMemory assembles the long-term memory wiring for one run: the system
// prompt the agent sends, the recorder the remember tool writes through, and
// the provider whose startup notices reach the user. With memory disabled the
// recorder and provider come back nil and only the base prompt is sent.
//
// It is a function rather than inline setup so the disabled path is testable —
// see the nil-interface note below, which is not something a reviewer can
// check by reading main().
func buildMemory(cwd string, cfg config.Config) (*memory.SystemPrompt, tools2.MemoryRecorder, *memory.Provider, []string) {
	var (
		recorder *memory.Recorder
		provider *memory.Provider
		notices  []string
	)
	systemPrompt := memory.NewSystemPrompt(prompts.SystemBase, prompts.SystemMemory, nil)

	if cfg.Memory.Enabled {
		mc := memory.DetectContext(cwd)
		// Report the detected scope. Org-scoped memories have to name this
		// value exactly, and it is derived from the git remote rather than
		// configured, so without saying it there is nothing to match against.
		if mc.Org != "" {
			notices = append(notices, fmt.Sprintf("Memory scope: org %q, tags %v.", mc.Org, mc.Tags))
		} else {
			notices = append(notices, fmt.Sprintf(
				"Memory scope: no organization detected (no git remote), tags %v. Only unscoped memories apply here.", mc.Tags))
		}
		store := memory.NewMergedStore(
			memory.NewFileStore(cfg.Memory.Dir),
			memory.NewFileStore(filepath.Join(config.ProjectRoot(cwd), ".dewey", "memory")),
		)
		provider = memory.NewProvider(store, memory.TagRetriever{}, mc, cfg.Memory.MaxTokens)
		systemPrompt = memory.NewSystemPrompt(prompts.SystemBase, prompts.SystemMemory, provider)
		recorder = memory.NewRecorder(store, mc)
	}

	// Two steps on purpose: assigning a nil *memory.Recorder straight into the
	// interface yields a non-nil interface holding a nil pointer, so the tool
	// factory would register a remember tool that panics on first use.
	var toolRecorder tools2.MemoryRecorder
	if recorder != nil {
		toolRecorder = recorder
	}
	return systemPrompt, toolRecorder, provider, notices
}

func initModel(agt agent.Agent, notices []string) *app.App {
	conversation := viewport.New()

	entry := textinput.New()
	entry.Prompt = "> "
	entry.SetVirtualCursor(false)

	return &app.App{Conversation: conversation, Entry: entry, Agent: agt, Notices: notices}
}
