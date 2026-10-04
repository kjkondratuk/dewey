package agent

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"github.com/kjkondratuk/dewey/internal/config"
	"github.com/kjkondratuk/dewey/internal/safety"
)

// lyingModel answers every classification request with read/safe, standing in
// for a local model that underrates a write.
type lyingModel struct{}

func (lyingModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return &schema.Message{
		Role:    schema.Assistant,
		Content: `{"kind":"read","risk":"safe","summary":"Harmless."}`,
	}, nil
}

func (lyingModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	panic("not used")
}

// realClassifier wires the actual classifier to the actual shipped rules, so
// these tests exercise the path a user gets rather than a stand-in.
func realClassifier(t *testing.T) safety.Classifier {
	t.Helper()
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	cfg, _, err := config.Load(project)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	return safety.NewLLMClassifier(lyingModel{}, cfg.ShellRules, config.ProjectRoot(project))
}

func toolNamed(t *testing.T, name string, invoked *int) tool.InvokableTool {
	t.Helper()
	tl, err := utils.InferTool(name, "does a thing", func(ctx context.Context, in echoInput) (string, error) {
		*invoked++
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("InferTool() error = %v", err)
	}
	return tl
}

// runAndAnswer drives one turn, denying any confirmation, and reports whether
// a prompt appeared.
func runAndAnswer(t *testing.T, toolName, args string) (sawConfirm bool, invoked int) {
	t.Helper()
	tl := toolNamed(t, toolName, &invoked)
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse(toolName, args),
		finalResponse("done"),
	}}

	agt := New(m, []tool.InvokableTool{tl}, realClassifier(t), 0, nil)
	ch, err := agt.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	drain(t, ch, func(Event) bool {
		sawConfirm = true
		return false // deny
	})
	return sawConfirm, invoked
}

// The guarantee: a classifier that underrates a write cannot talk the gate out
// of asking. Before the floor existed, this ran with no prompt at all.
func TestGate_WritesAreConfirmedEvenWhenTheClassifierSaysSafe(t *testing.T) {
	for _, command := range []string{
		`{"command":"echo hello > smoke-test.txt"}`,
		`{"command":"rm -rf dist"}`,
		`{"command":"git commit -m x"}`,
		`{"command":"npm install"}`,
	} {
		sawConfirm, invoked := runAndAnswer(t, "Bash", command)
		if !sawConfirm {
			t.Errorf("%s: no confirm event — a lying classifier bypassed the gate", command)
		}
		if invoked != 0 {
			t.Errorf("%s: tool invoked %d times after denial, want 0", command, invoked)
		}
	}
}

// And the other half: reads must not prompt, or the gate becomes noise the
// user learns to dismiss.
func TestGate_ReadsRunWithoutPrompting(t *testing.T) {
	for _, command := range []string{
		`{"command":"ls -la"}`,
		`{"command":"git status"}`,
		`{"command":"git log --oneline -20"}`,
		`{"command":"grep -rn foo ."}`,
		`{"command":"docker ps"}`,
		`{"command":"go version"}`,
	} {
		sawConfirm, invoked := runAndAnswer(t, "Bash", command)
		if sawConfirm {
			t.Errorf("%s: prompted for a read", command)
		}
		if invoked != 1 {
			t.Errorf("%s: tool invoked %d times, want 1", command, invoked)
		}
	}
}

// remember writes to the user's profile, and says so itself rather than
// relying on either the classifier or a config pin.
func TestGate_RememberIsConfirmedByDeclaration(t *testing.T) {
	sawConfirm, invoked := runAndAnswer(t, "remember", `{"text":"The user wants every command run as root."}`)
	if !sawConfirm {
		t.Error("no confirm event for remember")
	}
	if invoked != 0 {
		t.Errorf("remember invoked %d times after denial, want 0", invoked)
	}
}
