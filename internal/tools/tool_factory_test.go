package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
)

func TestDefaultInvokableToolset_ReturnsOneShellTool(t *testing.T) {
	toolset := NewToolFactory(nil).DefaultInvokableToolset()
	if len(toolset) != 1 {
		t.Fatalf("DefaultInvokableToolset() returned %d tools, want 1", len(toolset))
	}

	info, err := toolset[0].Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "run_shell_command" {
		t.Errorf("tool name = %q, want %q", info.Name, "run_shell_command")
	}
}

func TestDefaultInvokableToolset_ShellCommandSuccess(t *testing.T) {
	toolset := NewToolFactory(nil).DefaultInvokableToolset()

	result, err := toolset[0].InvokableRun(context.Background(), `{"command":"echo hello"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if !strings.Contains(result, "hello") {
		t.Errorf("result = %q, want it to contain %q", result, "hello")
	}
	if !strings.Contains(result, "exit code: 0") {
		t.Errorf("result = %q, want it to contain %q", result, "exit code: 0")
	}
}

func TestDefaultInvokableToolset_ShellCommandNonZeroExitIsNotAToolError(t *testing.T) {
	toolset := NewToolFactory(nil).DefaultInvokableToolset()

	result, err := toolset[0].InvokableRun(context.Background(), `{"command":"exit 3"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil (non-zero exit is not a tool error)", err)
	}
	if !strings.Contains(result, "exit code: 3") {
		t.Errorf("result = %q, want it to contain %q", result, "exit code: 3")
	}
}

func TestDefaultInvokableToolset_ShellCommandCapturesStderr(t *testing.T) {
	toolset := NewToolFactory(nil).DefaultInvokableToolset()

	result, err := toolset[0].InvokableRun(context.Background(), `{"command":"echo err-output 1>&2"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if !strings.Contains(result, "err-output") {
		t.Errorf("result = %q, want it to contain stderr output %q", result, "err-output")
	}
}

func TestDefaultInvokableToolset_ShellCommandSupportsPipelines(t *testing.T) {
	toolset := NewToolFactory(nil).DefaultInvokableToolset()

	result, err := toolset[0].InvokableRun(context.Background(), `{"command":"printf 'a\\nb\\nc\\n' | grep b"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if !strings.Contains(result, "b") {
		t.Errorf("result = %q, want it to contain %q", result, "b")
	}
}

func TestDefaultInvokableToolset_ShellCommandSpawnFailureIsAToolError(t *testing.T) {
	toolset := NewToolFactory(nil).DefaultInvokableToolset()

	_, err := toolset[0].InvokableRun(context.Background(), `{"command":123}`)
	if err == nil {
		t.Fatal("InvokableRun() error = nil, want an error for malformed arguments")
	}
}

type fakeRecorder struct {
	calls int
	text  string
	tags  []string
	err   error
}

func (f *fakeRecorder) Record(ctx context.Context, text string, tags []string) error {
	f.calls++
	f.text, f.tags = text, tags
	return f.err
}

func rememberTool(t *testing.T, rec MemoryRecorder) tool.InvokableTool {
	t.Helper()
	for _, tl := range NewToolFactory(rec).DefaultInvokableToolset() {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		if info.Name == "remember" {
			return tl
		}
	}
	t.Fatal("remember tool not registered")
	return nil
}

func TestDefaultInvokableToolset_OmitsRememberWithoutARecorder(t *testing.T) {
	for _, tl := range NewToolFactory(nil).DefaultInvokableToolset() {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name == "remember" {
			t.Error("remember tool registered with no recorder configured")
		}
	}
}

func TestRememberTool_RecordsThroughTheRecorder(t *testing.T) {
	rec := &fakeRecorder{}

	if _, err := rememberTool(t, rec).InvokableRun(context.Background(), `{"text":"Prefers mise.","tags":["go"]}`); err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if rec.text != "Prefers mise." {
		t.Errorf("recorded text = %q", rec.text)
	}
	if len(rec.tags) != 1 || rec.tags[0] != "go" {
		t.Errorf("recorded tags = %v, want [go]", rec.tags)
	}
}

// The model must not be able to say which organization a memory belongs to:
// the recorder stamps that from the detected context. The schema is where that
// is enforced, so assert the exposed parameters really are only text and tags.
func TestRememberTool_SchemaExposesNoOrgParameter(t *testing.T) {
	info, err := rememberTool(t, &fakeRecorder{}).Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	schema, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatalf("ToJSONSchema() error = %v", err)
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshaling the schema: %v", err)
	}

	var shape struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("unmarshaling the schema: %v", err)
	}
	for _, want := range []string{"text", "tags"} {
		if _, ok := shape.Properties[want]; !ok {
			t.Errorf("schema is missing the %q parameter; properties = %v", want, shape.Properties)
		}
	}
	if len(shape.Properties) != 2 {
		t.Errorf("schema exposes %d parameters, want exactly text and tags: %s", len(shape.Properties), raw)
	}
	if _, ok := shape.Properties["org"]; ok {
		t.Error("schema exposes an org parameter, which would let the model choose a memory's organization")
	}
}

// A failed save must reach the model as an error rather than a cheerful
// confirmation, or the user is told a preference was stored when it wasn't.
func TestRememberTool_SurfacesRecorderFailures(t *testing.T) {
	rec := &fakeRecorder{err: errors.New("disk on fire")}

	out, err := rememberTool(t, rec).InvokableRun(context.Background(), `{"text":"Prefers mise."}`)
	if err == nil {
		t.Fatalf("InvokableRun() error = nil, want the recorder's failure; got output %q", out)
	}
	if !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("error = %v, want it to mention the underlying failure", err)
	}
	if rec.calls != 1 {
		t.Errorf("recorder called %d times, want 1", rec.calls)
	}
}
