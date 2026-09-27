package main

import (
	"context"
	"log"
	"os"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/cloudwego/eino/schema"
	"github.com/kjkondratuk/dewey/internal/agent"
	"github.com/kjkondratuk/dewey/internal/app"
	"github.com/kjkondratuk/dewey/internal/compaction"
	"github.com/kjkondratuk/dewey/internal/config"
	"github.com/kjkondratuk/dewey/internal/safety"
	tools2 "github.com/kjkondratuk/dewey/internal/tools"
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

	// register tools
	tools := tools2.NewToolFactory().DefaultInvokableToolset()
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

	agt := agent.New(tm, tools, classifier, cfg.Overrides, contextWindow, summarizer)

	prog := tea.NewProgram(initModel(agt))
	_, err = prog.Run()
	if err != nil {
		log.Fatalf("program exited with error: %v", err)
	}
}

func initModel(agt agent.Agent) *app.App {
	conversation := viewport.New()

	entry := textinput.New()
	entry.Prompt = "> "
	entry.SetVirtualCursor(false)

	return &app.App{Conversation: conversation, Entry: entry, Agent: agt}
}
