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
	classifier := NewLLMClassifier(&fakeGenerateModel{content: `{"kind":"read","risk":"safe"}`})

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
	})

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
	classifier := NewLLMClassifier(&fakeGenerateModel{err: errors.New("connection refused")})

	got, err := classifier.Classify(context.Background(), "web_request", "makes HTTP requests", `{}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got != failSafeClassification {
		t.Errorf("Classify() = %+v, want fail-safe %+v", got, failSafeClassification)
	}
}

func TestLLMClassifier_MalformedJSONFailsSafe(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{content: "not json at all"})

	got, err := classifier.Classify(context.Background(), "web_request", "makes HTTP requests", `{}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got != failSafeClassification {
		t.Errorf("Classify() = %+v, want fail-safe %+v", got, failSafeClassification)
	}
}

func TestLLMClassifier_UnrecognizedFieldsFailSafe(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{content: `{"kind":"maybe","risk":"safe"}`})

	got, err := classifier.Classify(context.Background(), "web_request", "makes HTTP requests", `{}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got != failSafeClassification {
		t.Errorf("Classify() = %+v, want fail-safe %+v", got, failSafeClassification)
	}
}

func TestLLMClassifier_MalformedJSONInsideBracesFailsSafe(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{content: `{"kind":"read","risk":}`})

	got, err := classifier.Classify(context.Background(), "web_request", "makes HTTP requests", `{}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got != failSafeClassification {
		t.Errorf("Classify() = %+v, want fail-safe %+v", got, failSafeClassification)
	}
}

func TestLLMClassifier_UnrecognizedRiskFailsSafe(t *testing.T) {
	classifier := NewLLMClassifier(&fakeGenerateModel{content: `{"kind":"read","risk":"catastrophic"}`})

	got, err := classifier.Classify(context.Background(), "web_request", "makes HTTP requests", `{}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got != failSafeClassification {
		t.Errorf("Classify() = %+v, want fail-safe %+v", got, failSafeClassification)
	}
}
