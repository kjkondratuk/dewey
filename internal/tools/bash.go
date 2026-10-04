package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

type ShellCommandInput struct {
	Command string `json:"command" jsonschema:"required,description=The shell command to execute via sh -c"`
}

// newBashTool builds the shell tool. The name matches Claude Code's, which is
// what imported skills and agent tool allowlists refer to; safety.shellToolName
// has to agree with it or the deterministic shell floor stops applying.
func newBashTool() tool.InvokableTool {
	t, _ := utils.InferTool(
		"Bash",
		"Executes a shell command via sh -c and returns its combined stdout+stderr output plus exit code. Use this for running commands, builds, and tests. Prefer Read, Write, Edit, Grep, and Glob for file operations.",
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
	return t
}
