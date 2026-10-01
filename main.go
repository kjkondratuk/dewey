package main

import (
	"context"
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

	cfg, err := config.Load(cwd)
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
	classifier := safety.NewLLMClassifier(simpleModel)
	summarizer := compaction.NewLLMSummarizer(simpleModel)
	contextWindow := config.DiscoverContextWindow(ctx, cfg.Intermediate)

	systemPrompt, toolRecorder, _ := buildMemory(cwd, cfg)

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

	agt := agent.New(tm, tools, classifier, cfg.Overrides, contextWindow, summarizer).
		WithSystemPrompt(systemPrompt)

	prog := tea.NewProgram(initModel(agt))
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
func buildMemory(cwd string, cfg config.Config) (*memory.SystemPrompt, tools2.MemoryRecorder, *memory.Provider) {
	var (
		recorder *memory.Recorder
		provider *memory.Provider
	)
	systemPrompt := memory.NewSystemPrompt(prompts.SystemBase, nil)

	if cfg.Memory.Enabled {
		mc := memory.DetectContext(cwd, cfg.Org)
		store := memory.NewMergedStore(
			memory.NewFileStore(cfg.Memory.Dir),
			memory.NewFileStore(filepath.Join(cwd, ".dewey", "memory")),
		)
		provider = memory.NewProvider(store, memory.TagRetriever{}, mc, cfg.Memory.MaxTokens)
		systemPrompt = memory.NewSystemPrompt(prompts.SystemBase, provider)
		recorder = memory.NewRecorder(store, mc)
	}

	// Two steps on purpose: assigning a nil *memory.Recorder straight into the
	// interface yields a non-nil interface holding a nil pointer, so the tool
	// factory would register a remember tool that panics on first use.
	var toolRecorder tools2.MemoryRecorder
	if recorder != nil {
		toolRecorder = recorder
	}
	return systemPrompt, toolRecorder, provider
}

func initModel(agt agent.Agent) *app.App {
	conversation := viewport.New()

	entry := textinput.New()
	entry.Prompt = "> "
	entry.SetVirtualCursor(false)

	return &app.App{Conversation: conversation, Entry: entry, Agent: agt}
}
