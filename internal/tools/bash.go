package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/kjkondratuk/dewey/internal/safety"
)

type ShellCommandInput struct {
	Command string `json:"command" jsonschema:"required,description=The shell command to execute via sh -c. Only for work no other tool covers - running commands, builds, tests and git. A command a specific tool covers is refused"`
}

// newBashTool builds the shell tool. The name matches Claude Code's, which is
// what imported skills and agent tool allowlists refer to; safety.shellToolName
// has to agree with it or the deterministic shell floor stops applying.
func newBashTool() tool.InvokableTool {
	t, _ := utils.InferTool(
		"Bash",
		"Executes a shell command via sh -c and returns its combined stdout+stderr output plus exit code. "+
			"Bash is the fallback for work no other tool covers: running commands, builds, tests and git. "+
			"A command that a specific tool already covers is refused, so use Read to read a file, Write to "+
			"create one, Edit to change one, Grep to search contents, and Glob to find files by name.",
		runShell,
	)
	return t
}

// runShell refuses a command a purpose-built tool covers, then runs whatever
// is left. The refusal is structural rather than advisory because the
// advisory version -- in this description, and in the system prompt -- was
// already there and already ignored; see safety.PreferredTool.
func runShell(ctx context.Context, in ShellCommandInput) (string, error) {
	if better := safety.PreferredTool(in.Command); better != nil {
		return "", fmt.Errorf(
			"refused: %s Bash is a fallback for what no other tool covers, so call %s instead. "+
				"This is not a broken capability and retrying the same command will not help",
			better.Reason, better.Tool)
	}

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
}
