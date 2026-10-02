package safety

import (
	"encoding/json"
	"strings"
)

// globalFlagsWithValues are flags accepted before a subcommand that consume the
// token after them, so the subcommand isn't mistaken for their argument.
// Narrow and structural rather than configurable: these are properties of each
// CLI's own argument grammar, not policy.
var globalFlagsWithValues = map[string]map[string]bool{
	"git":       {"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true},
	"kubectl":   {"-n": true, "--namespace": true, "--context": true, "--kubeconfig": true, "--cluster": true},
	"helm":      {"-n": true, "--namespace": true, "--kube-context": true, "--kubeconfig": true},
	"docker":    {"-H": true, "--host": true, "--context": true, "--config": true},
	"podman":    {"--remote": true, "--connection": true},
	"systemctl": {"-H": true, "--host": true, "-M": true, "--machine": true},
	"terraform": {"-chdir": true},
}

// commandSeparators split a shell line into separately-executed commands. A
// mutating program anywhere in the pipeline counts, not just at the start.
var commandSeparators = []string{"&&", "||", ";", "|", "\n"}

// wrapperPrograms run another command; the real program is whatever follows.
var wrapperPrograms = set("env", "command", "exec", "time", "nohup", "builtin")

// ToolFloor returns the minimum classification for any tool call. For shell
// commands it inspects the command; for other tools it consults
// AlwaysWriteTools.
//
// This exists because the confirmation gate must not depend solely on a local
// model's judgement. The classifier is a language model asked to reason about
// consequences, and it demonstrably rates `echo hello > f.txt` as "safe". For
// a tool that can run anything, a structural check the classifier may raise
// but never lower is worth more than a better prompt.
//
// It is a floor, not a parser and not a security boundary. It does not expand
// variables, follow aliases, read scripts being invoked, or understand
// `sh -c "$CMD"`. Anything it misses still goes to the classifier.
func (r ShellRules) ToolFloor(toolName, argsJSON string) Classification {
	for _, name := range r.AlwaysWriteTools {
		if name == toolName {
			return Classification{Kind: "write", Risk: Moderate}
		}
	}
	if toolName != shellToolName {
		return Classification{}
	}
	return r.ShellFloor(argsJSON)
}

// ShellFloor returns the minimum classification for a shell command.
func (r ShellRules) ShellFloor(argsJSON string) Classification {
	command, ok := shellCommandFrom(argsJSON)
	if !ok {
		return Classification{}
	}

	for _, segment := range splitCommands(command) {
		if writesViaRedirection(segment) || r.segmentWrites(segment) {
			return Classification{Kind: "write", Risk: Moderate}
		}
	}
	return Classification{}
}

// shellCommandFrom pulls the command text out of the tool's JSON arguments.
func shellCommandFrom(argsJSON string) (string, bool) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", false
	}
	return args.Command, args.Command != ""
}

func splitCommands(command string) []string {
	segments := []string{command}
	for _, sep := range commandSeparators {
		var next []string
		for _, segment := range segments {
			next = append(next, strings.Split(segment, sep)...)
		}
		segments = next
	}
	return segments
}

// writesViaRedirection reports whether a segment redirects output to a file.
// Descriptor duplications like 2>&1 move an existing stream rather than
// creating anything, so they're stripped before looking.
func writesViaRedirection(segment string) bool {
	cleaned := segment
	for _, dup := range []string{"2>&1", "1>&2", ">&1", ">&2", "2>&-", ">&-"} {
		cleaned = strings.ReplaceAll(cleaned, dup, "")
	}
	return strings.Contains(cleaned, ">")
}

// segmentWrites decides whether one command in a pipeline changes state.
func (r ShellRules) segmentWrites(segment string) bool {
	fields := strings.Fields(segment)

	// Step past env assignments and wrappers so `FOO=1 env rm x` still
	// resolves to rm.
	i := 0
	for i < len(fields) {
		f := fields[i]
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") {
			i++
			continue
		}
		if wrapperPrograms[f] {
			i++
			continue
		}
		break
	}
	if i >= len(fields) {
		return false
	}

	program := baseName(fields[i])
	args := fields[i+1:]

	for _, name := range r.AlwaysWrite {
		if name == program {
			return true
		}
	}
	if rules, ok := r.ReadsWhen[program]; ok {
		return !r.readOnlyInvocation(program, rules, args)
	}
	if flags, ok := r.WritesWithFlags[program]; ok {
		return hasAnyFlag(args, flags)
	}
	return false
}

func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// readOnlyInvocation reports whether this call of a subcommand-shaped program
// only reads. Patterns are tried first, since they exist precisely to describe
// shapes a subcommand name cannot.
func (r ShellRules) readOnlyInvocation(program string, rules ProgramRules, args []string) bool {
	rest := strings.TrimSpace(strings.Join(args, " "))
	for _, re := range r.compiled[program] {
		if re.MatchString(rest) {
			return true
		}
	}

	sub, _, found := subcommandOf(program, args)
	if !found {
		// A bare `git` or `docker` prints usage.
		return true
	}
	for _, name := range rules.Subcommands {
		if name == sub {
			return true
		}
	}
	return false
}

// subcommandOf finds the subcommand, stepping over global flags and the values
// they consume.
func subcommandOf(program string, args []string) (sub string, rest []string, found bool) {
	withValues := globalFlagsWithValues[program]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a, args[i+1:], true
		}
		// --flag=value carries its own value; --flag value consumes the next.
		if withValues[a] && !strings.Contains(a, "=") {
			i++
		}
	}
	return "", nil, false
}

func hasAnyFlag(args []string, flags []string) bool {
	for _, a := range args {
		base := a
		if i := strings.Index(base, "="); i >= 0 {
			base = base[:i]
		}
		for _, f := range flags {
			if base == f {
				return true
			}
			// Clustered short flags: -xzf contains -x.
			if len(f) == 2 && strings.HasPrefix(f, "-") && !strings.HasPrefix(base, "--") &&
				strings.HasPrefix(base, "-") && strings.ContainsRune(base[1:], rune(f[1])) {
				return true
			}
		}
	}
	return false
}
