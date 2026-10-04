package safety

import (
	"encoding/json"
	"os"
	"path/filepath"
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
//
// Not the only entry point any more: AlwaysRead handles dewey's read-only
// tools, which skip classification entirely rather than receiving a floor.
// A tool in neither place is classified by the model alone.
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

// readOnlyTools maps each of dewey's read-only tools to the argument keys that
// together determine what it will actually touch. Order matters: the first key
// is a directory or file, and any later key is joined onto it.
//
// Glob needs two. Its pattern is joined onto its path by the tool itself, and
// filepath.Join cleans "..", so confining the path alone leaves the pattern
// free to walk straight out of the project — which is precisely how the
// confinement was first defeated.
var readOnlyTools = map[string][]string{
	"Read": {"file_path"},
	"Grep": {"path"},
	"Glob": {"path", "pattern"},
}

// AlwaysRead reports whether toolName is one of dewey's read-only tools
// operating entirely inside root, in which case the classifier is skipped and
// the call is treated as read/safe.
//
// Confinement is the whole basis for lowering a classification here. "Cannot
// write" is not the same as "cannot harm": an unconfined reader can lift a
// private key or an API token into the model's context, which is exactly the
// safe-kind/high-risk case RequiresApproval exists to catch. Inside the
// project the user is already working on, a read is genuinely unremarkable.
// Outside it, this returns false and the call goes to the classifier like any
// other — the read still happens, it just has to be approved.
//
// Fails closed on anything it cannot establish: unknown tool, unreadable
// path, unresolvable root, malformed arguments.
func (r ShellRules) AlwaysRead(toolName, argsJSON, root string) bool {
	if toolName == "" || toolName == shellToolName || root == "" {
		return false
	}
	// An explicit always-write pin outranks the fast path, so a user can
	// force a prompt on a tool dewey considers read-only.
	for _, name := range r.AlwaysWriteTools {
		if name == toolName {
			return false
		}
	}

	keys, ok := readOnlyTools[toolName]
	if !ok {
		return false
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return false
	}

	// The first key names a real directory or file, so it is resolved through
	// symlinks and confined. Grep and Glob default it to the working
	// directory, matching the tools' own behavior.
	base, _ := args[keys[0]].(string)
	if base == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return false
		}
		base = cwd
	}
	if !withinRoot(base, root) {
		return false
	}

	// Any remaining key is a pattern joined onto that base. It cannot be
	// resolved through symlinks because it does not name an existing path, so
	// it is checked lexically on the cleaned join — which is exactly the
	// operation the tool will perform.
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return false
	}
	target := resolvedBase
	for _, k := range keys[1:] {
		v, _ := args[k].(string)
		if v == "" {
			continue
		}
		target = filepath.Join(target, v)
	}
	if target == resolvedBase {
		return true
	}
	return withinRootLexical(target, root)
}

// withinRoot reports whether path resolves inside root. Both sides go through
// EvalSymlinks first: without it a link inside the project pointing at
// ~/.ssh would inherit the fast path, making the confinement decorative. It
// also makes macOS's /tmp -> /private/tmp agree with itself.
func withinRoot(path, root string) bool {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return false
	}

	// A root broad enough to contain the home directory is not a project, and
	// treating it as one would undo this whole function. ProjectRoot stops its
	// upward walk at a .dewey directory, so a ~/.dewey — which exists for
	// anyone who has saved a memory — makes every non-git directory under home
	// resolve to $HOME itself. That would put ~/.ssh, ~/.aws and ~/.gnupg
	// inside "the project" and back on the no-prompt path.
	if isTooBroad(resolvedRoot) {
		return false
	}

	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// withinRootLexical reports whether a cleaned path lies inside root without
// requiring it to exist. A glob pattern names no real file, so EvalSymlinks
// cannot be used on it — but filepath.Join has already applied the same
// cleaning the tool will apply, so comparing the cleaned result is checking
// the operation that will actually happen.
//
// Only safe because the base directory it was joined onto was resolved and
// confined first; on its own it would be fooled by a symlinked base.
func withinRootLexical(path, root string) bool {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	if isTooBroad(resolvedRoot) {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(resolvedRoot, filepath.Clean(abs))
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// isTooBroad reports whether root is the home directory or an ancestor of it.
// Fails closed: a home directory that cannot be determined or resolved is
// treated as broad, because the alternative is granting the fast path on the
// strength of a path lookup that just failed.
func isTooBroad(resolvedRoot string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return true
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return true
	}

	// root contains (or equals) home => root is at least as broad as home.
	rel, err := filepath.Rel(resolvedRoot, resolvedHome)
	if err != nil {
		return true
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
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
