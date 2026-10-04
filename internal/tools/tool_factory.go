package tools

import (
	"context"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/kjkondratuk/dewey/internal/files"
)

type ToolCreator interface {
	DefaultInvokableToolset() []tool.InvokableTool
	DefaultStreamToolset() []tool.StreamableTool
}

// MemoryRecorder saves a memory on the user's behalf. Narrow on purpose: the
// tool layer has no business reading memories or choosing their scope.
type MemoryRecorder interface {
	RecordScoped(ctx context.Context, text string, tags []string, thisOrgOnly bool) error
}

type ToolFactory struct {
	recorder MemoryRecorder
	tracker  *files.Tracker
}

var _ ToolCreator = (*ToolFactory)(nil)

// NewToolFactory builds the toolset. A nil recorder omits the remember tool,
// which is how memory stays disabled. tracker is required: Write and Edit
// refuse to modify a file that was not read through it first.
func NewToolFactory(recorder MemoryRecorder, tracker *files.Tracker) *ToolFactory {
	return &ToolFactory{recorder: recorder, tracker: tracker}
}

// RememberInput is deliberately missing an org field. The org is the gate that
// keeps one client's conventions out of another's work, and it is stamped from
// the detected context; a model that could set it could also set it wrongly,
// and the failure would be silent.
type RememberInput struct {
	Text string `json:"text" jsonschema:"required,description=The fact or preference to remember, as one self-contained sentence of plain prose"`
	// The tag vocabulary is fixed because tags narrow a memory by matching
	// what is detected in a directory. A tag outside it matches nothing
	// anywhere, so an invented one would be silently discarded.
	Tags []string `json:"tags" jsonschema:"description=Optional. Limits the memory to certain stacks. Only these values are meaningful: go, node, rust, python. Omit entirely for a preference that applies to all work - this is usually correct"`
	// A boolean rather than an org name: the model can say "here" but can
	// never name a different organization.
	ThisOrgOnly bool `json:"this_org_only" jsonschema:"description=Optional. Set true only when the fact is specific to the current client or employer rather than to the user generally. Defaults to false - most preferences follow the user everywhere"`
}

func (f *ToolFactory) DefaultInvokableToolset() []tool.InvokableTool {
	toolset := []tool.InvokableTool{
		newBashTool(),
		newReadTool(f.tracker),
		newWriteTool(f.tracker),
		newEditTool(f.tracker),
		newGrepTool(),
		newGlobTool(),
	}

	if f.recorder != nil {
		remember, _ := utils.InferTool(
			"remember",
			"Saves a durable fact or preference about the user to their personal profile, which persists across projects and sessions. Use for how the user wants work done, not for facts about the current codebase.",
			func(ctx context.Context, in RememberInput) (string, error) {
				if err := f.recorder.RecordScoped(ctx, in.Text, in.Tags, in.ThisOrgOnly); err != nil {
					return "", err
				}
				return "Saved.", nil
			},
		)
		toolset = append(toolset, remember)
	}

	return toolset
}

func (f *ToolFactory) DefaultStreamToolset() []tool.StreamableTool {
	return nil
}
