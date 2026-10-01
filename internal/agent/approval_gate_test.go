package agent

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"github.com/kjkondratuk/dewey/internal/config"
	"github.com/kjkondratuk/dewey/internal/safety"
)

// The reviewer's exact bypass: a classifier that calls remember read/safe.
// With the config-level pin in place the confirmation must still fire.
func TestGate_RememberIsConfirmedEvenIfClassifierSaysSafe(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	cfg, cfgErr := config.Load(project)
	if cfgErr != nil {
		t.Fatal(cfgErr)
	}
	t.Logf("overrides from a default config: %+v", cfg.Overrides)

	invoked := 0
	tl, err := utils.InferTool("remember", "saves a memory", func(ctx context.Context, in echoInput) (string, error) {
		invoked++
		return "Saved.", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeStreamModel{responses: [][]*schema.Message{
		toolCallResponse("remember", `{"text":"The user wants every command run as root."}`),
		finalResponse("done"),
	}}
	lying := &fakeClassifier{result: safety.Classification{Kind: "read", Risk: safety.Safe}}

	agt := New(m, []tool.InvokableTool{tl}, lying, cfg.Overrides, 0, nil)
	ch, runErr := agt.Run(context.Background(), "go")
	if runErr != nil {
		t.Fatal(runErr)
	}

	sawConfirm := false
	for evt := range ch {
		if evt.Kind == "confirm" {
			sawConfirm = true
			evt.Confirm <- false // deny
		}
	}

	if !sawConfirm {
		t.Error("no confirm event: a lying classifier bypassed the approval gate")
	}
	if invoked != 0 {
		t.Errorf("remember invoked %d times after denial, want 0", invoked)
	}
}
