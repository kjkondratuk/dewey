package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/cloudwego/eino/schema"
	"github.com/kjkondratuk/dewey/internal/agent"
	"github.com/kjkondratuk/dewey/internal/app"
	"github.com/kjkondratuk/dewey/internal/compaction"
	"github.com/kjkondratuk/dewey/internal/config"
	"github.com/kjkondratuk/dewey/internal/files"
	"github.com/kjkondratuk/dewey/internal/memory"
	"github.com/kjkondratuk/dewey/internal/picker"
	"github.com/kjkondratuk/dewey/internal/safety"
	"github.com/kjkondratuk/dewey/internal/session"
	tools2 "github.com/kjkondratuk/dewey/internal/tools"
	"github.com/kjkondratuk/dewey/prompts"
	"golang.org/x/term"
)

func main() {
	resume := flag.Bool("resume", false, "resume a previous session: pass an id (or unambiguous id prefix) as the first argument, or omit it to choose one interactively")
	listSessions := flag.Bool("sessions", false, "list recorded sessions and exit")
	noSession := flag.Bool("no-session", false, "do not record this session to disk")
	flag.Parse()

	if *listSessions {
		if err := printSessions(); err != nil {
			log.Fatalf("failed to list sessions: %v", err)
		}
		return
	}

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
	classifier := safety.NewLLMClassifier(simpleModel, cfg.ShellRules, config.ProjectRoot(cwd))
	summarizer := compaction.NewLLMSummarizer(simpleModel)
	contextWindow := config.DiscoverContextWindow(ctx, cfg.Intermediate)

	// A model that cannot call tools cannot do dewey's job; it will invent
	// tool results instead. Checked here rather than tolerated because the
	// failure mode is an unbounded stream of plausible fiction. Only the
	// intermediate tier is checked: it is the tier tools are bound to below
	// (intermediateModel.WithTools); simple only classifies and summarizes
	// and is never given tools.
	if err := checkToolSupport(ctx, cfg.Intermediate); err != nil {
		log.Fatal(err)
	}

	systemPrompt, toolRecorder, provider, notices := buildMemory(cwd, cfg)
	notices = append(ruleNotices, notices...)

	// Session recording is debugging instrumentation, not a feature the run
	// depends on: any problem standing it up becomes a notice, never a fatal
	// error (see buildSession / resumeSession).
	var (
		store       *session.Store
		seedHistory []*schema.Message
	)
	switch {
	case *noSession:
		// Recording disabled explicitly; store stays nil.
	case *resume:
		id, err := resolveResumeID(flag.Arg(0))
		if errors.Is(err, errNoSessionsToResume) {
			fmt.Println("no recorded sessions")
			return
		}
		if err != nil {
			log.Fatal(err)
		}
		if id == "" {
			// The picker ran and the user quit without choosing -- a normal
			// outcome, not an error, so exit quietly.
			return
		}

		var sessionNotices []string
		store, seedHistory, sessionNotices, err = resumeSession(cwd, cfg, contextWindow, id)
		if err != nil {
			log.Fatalf("failed to resume session %q: %v", id, err)
		}
		notices = append(notices, sessionNotices...)
	default:
		var sessionNotices []string
		store, sessionNotices = buildSession(cwd, cfg, contextWindow)
		notices = append(notices, sessionNotices...)
	}

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
	// One tracker per session: Write and Edit consult it to refuse modifying a
	// file that was not read first.
	tracker := files.NewTracker()
	tools := tools2.NewToolFactory(toolRecorder, tracker).DefaultInvokableToolset()
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

	agtBuilder := agent.New(tm, tools, classifier, contextWindow, summarizer).
		WithSystemPrompt(systemPrompt).
		// Startup discovery runs before any request, so the model is often
		// not loaded yet and contextWindow above may be a clamped guess. Once
		// the provider serves a request the real window is observable.
		WithContextWindowRefresher(func(ctx context.Context) (int, bool) {
			return config.RefreshContextWindow(ctx, cfg.Intermediate)
		})
	if len(seedHistory) > 0 {
		agtBuilder = agtBuilder.WithHistory(seedHistory)
	}
	if store != nil {
		defer store.Close()
		agtBuilder = agtBuilder.WithRecorder(store)
	}
	agt := agtBuilder

	prog := tea.NewProgram(initModel(agt, notices))
	_, err = prog.Run()
	if err != nil {
		log.Fatalf("program exited with error: %v", err)
	}

	// Surface any recording problem only after the run so it never delays or
	// blocks the conversation itself -- a recording failure must never break
	// a session.
	if store != nil {
		if recErr := store.Err(); recErr != nil {
			log.Printf("session recording error: %v", recErr)
		}
	}
}

// checkToolSupport gates startup on the intermediate tier's ability to call
// tools, which dewey requires: a model that cannot call tools hallucinates
// tool results instead of running anything, and with no token limit can
// generate until the context window fills. Extracted from main() so the
// fail-open behavior on an unknown or merely-unreachable provider is
// testable -- an untested fatal path is how that requirement silently
// becomes fail-closed.
//
// Returns nil for config.ToolSupportYes and config.ToolSupportUnknown alike;
// only config.ToolSupportNo, a definite negative, is an error.
func checkToolSupport(ctx context.Context, m config.ModelConfig) error {
	if config.DiscoverToolSupport(ctx, m) != config.ToolSupportNo {
		return nil
	}
	alternatives := config.ToolCapableModels(ctx, m)
	msg := fmt.Sprintf("model %q does not support tool calling, which dewey requires", m.Model)
	if len(alternatives) > 0 {
		msg += fmt.Sprintf("\nmodels on this server that do: %s", strings.Join(alternatives, ", "))
	}
	msg += "\nset models.intermediate.model in ~/.dewey/config.yaml"
	return errors.New(msg)
}

// errNoSessionsToResume signals that --resume was given with no id and
// there is nothing recorded to choose from. It is distinct from every other
// resumeTarget error: callers treat it as "print a plain message and exit
// 0", not as a failure.
var errNoSessionsToResume = errors.New("no recorded sessions")

// resolveResumeID is the IO-performing wrapper around resumeTarget for
// --resume with no id already on the command line: it loads the recorded
// sessions, checks whether stdout is a real terminal, and -- via
// resumeTarget -- runs the interactive picker when one can be shown.
func resolveResumeID(id string) (string, error) {
	if id != "" {
		return id, nil
	}

	dir, err := sessionsDir()
	var sums []session.Summary
	if err == nil {
		sums, err = session.Summaries(dir)
		if err != nil {
			return "", err
		}
	}

	interactive := term.IsTerminal(int(os.Stdout.Fd()))
	return resumeTarget(id, sums, interactive, picker.Choose)
}

// resumeTarget decides which session id --resume should resolve to. It is a
// pure decision, with everything TTY-dependent injected (interactive,
// choose), so it is testable without a real terminal:
//
//   - an id already given is used as-is, without even looking at sums
//   - no id and no sessions is errNoSessionsToResume -- nothing to choose
//   - no id, sessions exist, but no terminal to run a picker in is a plain
//     error asking for an id instead of attempting a TUI that would panic
//   - no id and a terminal defers to choose; its result (including "",
//     which means the user quit without choosing -- not an error) is
//     returned unchanged
func resumeTarget(id string, sums []session.Summary, interactive bool, choose func([]session.Summary) (string, error)) (string, error) {
	if id != "" {
		return id, nil
	}
	if len(sums) == 0 {
		return "", errNoSessionsToResume
	}
	if !interactive {
		return "", errors.New("--resume needs a session id when stdout is not a terminal: pass one, e.g. dewey --resume <id>")
	}
	return choose(sums)
}

// sessionsDir is where session transcripts live: ~/.dewey/sessions. It is a
// function (not a package-level constant) so tests can point it at a
// temporary $HOME without touching the real one.
func sessionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determining home directory: %w", err)
	}
	return filepath.Join(home, ".dewey", "sessions"), nil
}

// buildSession starts a new session transcript for this run. Recording is
// instrumentation, not a dependency of the conversation: if the home
// directory can't be found or the sessions directory can't be created, it
// returns a nil store and a notice explaining why, rather than failing the
// run.
func buildSession(cwd string, cfg config.Config, window int) (*session.Store, []string) {
	dir, err := sessionsDir()
	if err != nil {
		return nil, []string{fmt.Sprintf("session recording disabled: %v", err)}
	}

	store, err := session.NewStore(dir, session.Header{
		CWD:               cwd,
		ProjectRoot:       config.ProjectRoot(cwd),
		IntermediateModel: cfg.Intermediate.Model,
		SimpleModel:       cfg.Simple.Model,
		ContextWindow:     window,
	})
	if err != nil {
		return nil, []string{fmt.Sprintf("session recording disabled: %v", err)}
	}
	return store, nil
}

// resumeSession loads a prior session's transcript by id (or unambiguous
// prefix) and starts a new session file that continues it -- a resumed
// transcript is never appended to the original file, so the original stays a
// faithful record of exactly what happened in that run. Failing to resolve
// or load the requested id is returned as an error: the user asked to
// resume a specific conversation, so silently starting a fresh one instead
// would hide that their request did not do what they asked. Once the
// original is loaded, standing up the new session file is instrumentation
// like buildSession -- a failure there is a notice, not a fatal error.
func resumeSession(cwd string, cfg config.Config, window int, id string) (*session.Store, []*schema.Message, []string, error) {
	dir, err := sessionsDir()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("determining sessions directory: %w", err)
	}

	path, err := session.Resolve(dir, id)
	if err != nil {
		return nil, nil, nil, err
	}

	history, _, err := session.Load(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("loading session %q: %w", id, err)
	}

	store, err := session.NewStore(dir, session.Header{
		CWD:               cwd,
		ProjectRoot:       config.ProjectRoot(cwd),
		IntermediateModel: cfg.Intermediate.Model,
		SimpleModel:       cfg.Simple.Model,
		ContextWindow:     window,
		ResumedFrom:       id,
	})
	if err != nil {
		return nil, history, []string{fmt.Sprintf("session recording disabled: %v", err)}, nil
	}
	return store, history, nil, nil
}

// printSessions implements --sessions: an aligned table (id, start time,
// message count, project, opening message) with no TUI started. It uses the
// same session.Columns renderer as the interactive picker, so the two can
// never drift apart.
func printSessions() error {
	dir, err := sessionsDir()
	if err != nil {
		// No home directory, no sessions directory, nothing to list -- not a
		// failure worth reporting here.
		return nil
	}

	sums, err := session.Summaries(dir)
	if err != nil {
		return err
	}

	header, rows := session.Columns(sums, terminalWidth())
	fmt.Println(header)
	for _, row := range rows {
		fmt.Println(row)
	}
	return nil
}

// terminalWidth is the width printSessions renders its table at: the real
// terminal width when stdout is a terminal, else a sensible default so the
// listing still works headless (piped to a file, redirected in a script).
func terminalWidth() int {
	const fallback = 100
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	return fallback
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
