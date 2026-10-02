package safety

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type fakeGenerateModel struct {
	content string
	err     error
}

func (f *fakeGenerateModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &schema.Message{Role: schema.Assistant, Content: f.content}, nil
}

func (f *fakeGenerateModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	panic("not used in these tests")
}

func TestLLMClassifier_ValidResponse(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{content: `{"kind":"read","risk":"safe"}`}, testRules(t))

	got, err := classifier.Classify(context.Background(), "read_file", "reads a file", `{"path":"/tmp/x"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Classification{Kind: "read", Risk: Safe}
	if got != want {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestLLMClassifier_ToleratesSurroundingText(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{
		content: "Sure, here is my answer:\n```json\n{\"kind\":\"write\",\"risk\":\"dangerous\"}\n```",
	}, testRules(t))

	got, err := classifier.Classify(context.Background(), "web_request", "makes HTTP requests", `{"method":"DELETE"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Classification{Kind: "write", Risk: Dangerous}
	if got != want {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestLLMClassifier_ModelErrorFailsSafe(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{err: errors.New("connection refused")}, testRules(t))

	got, err := classifier.Classify(context.Background(), "web_request", "makes HTTP requests", `{}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got != failSafeClassification {
		t.Errorf("Classify() = %+v, want fail-safe %+v", got, failSafeClassification)
	}
}

func TestLLMClassifier_MalformedJSONFailsSafe(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{content: "not json at all"}, testRules(t))

	got, err := classifier.Classify(context.Background(), "web_request", "makes HTTP requests", `{}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got != failSafeClassification {
		t.Errorf("Classify() = %+v, want fail-safe %+v", got, failSafeClassification)
	}
}

func TestLLMClassifier_UnrecognizedFieldsFailSafe(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{content: `{"kind":"maybe","risk":"safe"}`}, testRules(t))

	got, err := classifier.Classify(context.Background(), "web_request", "makes HTTP requests", `{}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got != failSafeClassification {
		t.Errorf("Classify() = %+v, want fail-safe %+v", got, failSafeClassification)
	}
}

func TestLLMClassifier_MalformedJSONInsideBracesFailsSafe(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{content: `{"kind":"read","risk":}`}, testRules(t))

	got, err := classifier.Classify(context.Background(), "web_request", "makes HTTP requests", `{}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got != failSafeClassification {
		t.Errorf("Classify() = %+v, want fail-safe %+v", got, failSafeClassification)
	}
}

func TestLLMClassifier_UnrecognizedRiskFailsSafe(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{content: `{"kind":"read","risk":"catastrophic"}`}, testRules(t))

	got, err := classifier.Classify(context.Background(), "web_request", "makes HTTP requests", `{}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got != failSafeClassification {
		t.Errorf("Classify() = %+v, want fail-safe %+v", got, failSafeClassification)
	}
}

func TestLLMClassifier_ParsesAndTrimsSummary(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{
		content: `{"kind":"write","risk":"dangerous","summary":"  Deletes the build output directory.  "}`,
	}, testRules(t))

	got, err := classifier.Classify(context.Background(), "run_shell_command", "runs a shell command", `{"command":"rm -rf ./dist"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Classification{Kind: "write", Risk: Dangerous, Summary: "Deletes the build output directory."}
	if got != want {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

// Summary is cosmetic, so its absence must not drag the classification down
// the fail-safe path the way a bad kind or risk does.
func TestLLMClassifier_MissingSummaryIsNotFatal(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{content: `{"kind":"read","risk":"safe"}`}, testRules(t))

	got, err := classifier.Classify(context.Background(), "run_shell_command", "runs a shell command", `{"command":"ls"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Classification{Kind: "read", Risk: Safe}
	if got != want {
		t.Errorf("Classify() = %+v, want %+v (summary absent but kind/risk intact)", got, want)
	}
}

func TestLLMClassifier_EmptySummaryIsNotFatal(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{content: `{"kind":"write","risk":"moderate","summary":"   "}`}, testRules(t))

	got, err := classifier.Classify(context.Background(), "run_shell_command", "runs a shell command", `{"command":"touch f"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Risk != Moderate || got.Kind != "write" || got.Summary != "" {
		t.Errorf("Classify() = %+v, want write/moderate with an empty summary", got)
	}
}

// A summary must not rescue an otherwise-invalid classification.
func TestLLMClassifier_BadKindStillFailsSafeDespiteSummary(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{
		content: `{"kind":"maybe","risk":"safe","summary":"Lists files."}`,
	}, testRules(t))

	got, err := classifier.Classify(context.Background(), "run_shell_command", "runs a shell command", `{"command":"ls"}`)
	if err == nil {
		t.Fatal("expected an error for an unrecognized kind")
	}
	if got != failSafeClassification {
		t.Errorf("Classify() = %+v, want the fail-safe classification %+v", got, failSafeClassification)
	}
}

// testRules gives classifier tests the real built-in floor, so a test that
// passes here reflects what ships.
func testRules(t *testing.T) ShellRules {
	t.Helper()
	r, err := DefaultShellRules()
	if err != nil {
		t.Fatalf("DefaultShellRules() error = %v", err)
	}
	return r
}
