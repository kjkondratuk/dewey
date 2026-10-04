package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/kjkondratuk/dewey/internal/files"
	"github.com/kjkondratuk/dewey/internal/safety"
)

func TestDefaultInvokableToolset_ShellCommandSuccess(t *testing.T) {
	toolset := NewToolFactory(nil, files.NewTracker()).DefaultInvokableToolset()

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
	toolset := NewToolFactory(nil, files.NewTracker()).DefaultInvokableToolset()

	result, err := toolset[0].InvokableRun(context.Background(), `{"command":"exit 3"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil (non-zero exit is not a tool error)", err)
	}
	if !strings.Contains(result, "exit code: 3") {
		t.Errorf("result = %q, want it to contain %q", result, "exit code: 3")
	}
}

func TestDefaultInvokableToolset_ShellCommandCapturesStderr(t *testing.T) {
	toolset := NewToolFactory(nil, files.NewTracker()).DefaultInvokableToolset()

	result, err := toolset[0].InvokableRun(context.Background(), `{"command":"echo err-output 1>&2"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if !strings.Contains(result, "err-output") {
		t.Errorf("result = %q, want it to contain stderr output %q", result, "err-output")
	}
}

func TestDefaultInvokableToolset_ShellCommandSupportsPipelines(t *testing.T) {
	toolset := NewToolFactory(nil, files.NewTracker()).DefaultInvokableToolset()

	result, err := toolset[0].InvokableRun(context.Background(), `{"command":"printf 'a\\nb\\nc\\n' | grep b"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if !strings.Contains(result, "b") {
		t.Errorf("result = %q, want it to contain %q", result, "b")
	}
}

func TestDefaultInvokableToolset_ShellCommandSpawnFailureIsAToolError(t *testing.T) {
	toolset := NewToolFactory(nil, files.NewTracker()).DefaultInvokableToolset()

	_, err := toolset[0].InvokableRun(context.Background(), `{"command":123}`)
	if err == nil {
		t.Fatal("InvokableRun() error = nil, want an error for malformed arguments")
	}
}

type fakeRecorder struct {
	calls       int
	text        string
	tags        []string
	thisOrgOnly bool
	err         error
}

func (f *fakeRecorder) RecordScoped(ctx context.Context, text string, tags []string, thisOrgOnly bool) error {
	f.calls++
	f.text, f.tags, f.thisOrgOnly = text, tags, thisOrgOnly
	return f.err
}

func rememberTool(t *testing.T, rec MemoryRecorder) tool.InvokableTool {
	t.Helper()
	for _, tl := range NewToolFactory(rec, files.NewTracker()).DefaultInvokableToolset() {
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
	for _, tl := range NewToolFactory(nil, files.NewTracker()).DefaultInvokableToolset() {
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
	for _, want := range []string{"text", "tags", "this_org_only"} {
		if _, ok := shape.Properties[want]; !ok {
			t.Errorf("schema is missing the %q parameter; properties = %v", want, shape.Properties)
		}
	}
	if len(shape.Properties) != 3 {
		t.Errorf("schema exposes %d parameters, want exactly text, tags and this_org_only: %s", len(shape.Properties), raw)
	}
	// this_org_only can only ask for the org already detected. A field naming
	// one is what would let the model reach another client's memories.
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

// Omitting this_org_only must mean "applies everywhere", since that is what
// the model will do for the great majority of preferences.
func TestRememberTool_DefaultsToUniversalScope(t *testing.T) {
	rec := &fakeRecorder{}
	if _, err := rememberTool(t, rec).InvokableRun(context.Background(), `{"text":"Prefers conventional commits."}`); err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if rec.thisOrgOnly {
		t.Error("thisOrgOnly = true with the field omitted, want false")
	}
}

func TestRememberTool_PassesScopeThrough(t *testing.T) {
	rec := &fakeRecorder{}
	if _, err := rememberTool(t, rec).InvokableRun(context.Background(), `{"text":"Bills to cost center 7781.","this_org_only":true}`); err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if !rec.thisOrgOnly {
		t.Error("thisOrgOnly = false, want the requested scoping honored")
	}
}

func TestDefaultInvokableToolset_ExposesTheClaudeCoreTools(t *testing.T) {
	f := NewToolFactory(nil, files.NewTracker())

	have := map[string]bool{}
	for _, tl := range f.DefaultInvokableToolset() {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		have[info.Name] = true
	}

	for _, want := range []string{"Bash", "Read", "Write", "Edit", "Grep", "Glob"} {
		if !have[want] {
			t.Errorf("tool %q missing from the toolset", want)
		}
	}
	if have["remember"] {
		t.Error("remember present with a nil recorder, want it omitted")
	}
}

func TestDefaultInvokableToolset_EveryToolHasASafetyClassification(t *testing.T) {
	// A tool in neither list falls through to the classifier alone, which is
	// how a write ends up unprompted. This catches a tool added later without
	// a matching rule — the failure would otherwise be silent and only
	// visible as a missing approval prompt at runtime.
	rules, err := safety.DefaultShellRules()
	if err != nil {
		t.Fatalf("DefaultShellRules() error = %v", err)
	}

	// AlwaysRead needs a path argument that resolves inside root, and the key
	// differs per tool. A temp dir is used rather than $HOME because a root
	// that broad is rejected outright.
	root := t.TempDir()
	pathArgKey := map[string]string{
		"Read": "file_path",
		"Grep": "path",
		"Glob": "path",
	}

	f := NewToolFactory(nil, files.NewTracker())
	for _, tl := range f.DefaultInvokableToolset() {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		if info.Name == "Bash" {
			continue // classified by inspecting the command, not by name
		}

		if key, ok := pathArgKey[info.Name]; ok {
			args := `{"` + key + `":"` + root + `"}`
			if !rules.AlwaysRead(info.Name, args, root) {
				t.Errorf("tool %q is read-only but AlwaysRead(in-root) = false; it is missing from safety's readOnlyTools", info.Name)
			}
			continue
		}

		if floor := rules.ToolFloor(info.Name, "{}"); floor.Kind != "write" {
			t.Errorf("tool %q has no safety classification: not in always_write_tools and not a known read-only tool", info.Name)
		}
	}
}

func TestDefaultInvokableToolset_IncludesRememberWithARecorder(t *testing.T) {
	f := NewToolFactory(&fakeRecorder{}, files.NewTracker())

	have := map[string]bool{}
	for _, tl := range f.DefaultInvokableToolset() {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		have[info.Name] = true
	}
	if !have["remember"] {
		t.Error("remember missing with a recorder present")
	}
}

func TestRememberTool_TagVocabularyReachesTheModel(t *testing.T) {
	// A comma inside jsonschema:"description=..." truncates it at that comma.
	// This description listed "go, node, rust, python" and reached the model
	// as "...Only these values are meaningful: go" — so three of the four
	// valid tags were invisible, and the whole tag vocabulary the memory
	// system is built on was unusable. Nothing asserted otherwise, which is
	// why it survived.
	info, err := newRememberTool(&fakeRecorder{}).Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var parsed struct {
		JSONSchema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
			Required []string `json:"required"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	desc := parsed.JSONSchema.Properties["tags"].Description
	for _, want := range []string{"go", "node", "rust", "python"} {
		if !strings.Contains(desc, want) {
			t.Errorf("tags description = %q, want it to name the %q tag", desc, want)
		}
	}

	// Same truncation, same tool: the text description lost the guidance that
	// makes a saved memory well-formed.
	if got := parsed.JSONSchema.Properties["text"].Description; !strings.Contains(got, "self-contained") {
		t.Errorf("text description = %q, want it to keep the self-contained-sentence guidance", got)
	}

	// The description tells the model to omit tags for a universal
	// preference, so the schema must not contradict it by demanding one.
	for _, name := range parsed.JSONSchema.Required {
		if name == "tags" || name == "this_org_only" {
			t.Errorf("required = %v, want %q optional — its own description says to omit it", parsed.JSONSchema.Required, name)
		}
	}
}
