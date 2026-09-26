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

type ToolFactory struct {
}

var _ ToolCreator = (*ToolFactory)(nil)

func NewToolFactory() *ToolFactory {
	return &ToolFactory{}
}

type ShellCommandInput struct {
	Command string `json:"command" jsonschema:"required,description=The shell command to execute via sh -c"`
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

	return []tool.InvokableTool{
		shellCommand,
	}
}

func (f *ToolFactory) DefaultStreamToolset() []tool.StreamableTool {
	return nil
}
