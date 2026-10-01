package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

type ToolCreator interface {
	DefaultInvokableToolset() []tool.InvokableTool
	DefaultStreamToolset() []tool.StreamableTool
}

// MemoryRecorder saves a memory on the user's behalf. Narrow on purpose: the
// tool layer has no business reading memories or choosing their scope.
type MemoryRecorder interface {
	Record(ctx context.Context, text string, tags []string) error
}

type ToolFactory struct {
	recorder MemoryRecorder
}

var _ ToolCreator = (*ToolFactory)(nil)

// NewToolFactory builds the toolset. A nil recorder omits the remember tool,
// which is how memory stays disabled.
func NewToolFactory(recorder MemoryRecorder) *ToolFactory {
	return &ToolFactory{recorder: recorder}
}

type ShellCommandInput struct {
	Command string `json:"command" jsonschema:"required,description=The shell command to execute via sh -c"`
}

// RememberInput is deliberately missing an org field. The org is the gate that
// keeps one client's conventions out of another's work, and it is stamped from
// the detected context; a model that could set it could also set it wrongly,
// and the failure would be silent.
type RememberInput struct {
	Text string   `json:"text" jsonschema:"required,description=The fact or preference to remember, as one self-contained sentence of plain prose"`
	Tags []string `json:"tags" jsonschema:"description=Optional topical tags such as go or testing; omit if the memory applies to all work"`
}

func (f *ToolFactory) DefaultInvokableToolset() []tool.InvokableTool {
	shellCommand, _ := utils.InferTool(
		"run_shell_command",
		"Executes a shell command via sh -c and returns its combined stdout+stderr output plus exit code. Use this for reading/writing files, listing directories, making network requests, or any other command-line operation.",
		func(ctx context.Context, in ShellCommandInput) (string, error) {
			cmd := exec.CommandContext(ctx, "sh", "-c", in.Command)
			output, err := cmd.CombinedOutput()

			var exitErr *exec.ExitError
			if err != nil && !errors.As(err, &exitErr) {
				return "", err
			}

			exitCode := 0
			if exitErr != nil {
				exitCode = exitErr.ExitCode()
			}

			return fmt.Sprintf("%s\nexit code: %d", output, exitCode), nil
		},
	)

	toolset := []tool.InvokableTool{shellCommand}

	if f.recorder != nil {
		remember, _ := utils.InferTool(
			"remember",
			"Saves a durable fact or preference about the user to their personal profile, which persists across projects and sessions. Use for how the user wants work done, not for facts about the current codebase.",
			func(ctx context.Context, in RememberInput) (string, error) {
				if err := f.recorder.Record(ctx, in.Text, in.Tags); err != nil {
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
