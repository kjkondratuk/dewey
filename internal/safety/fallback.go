package safety

import "strings"

// Bash is a fallback: it runs anything, so a model under pressure reaches for
// it even where a purpose-built tool is better. Prose does not fix this. The
// system prompt already says "edit it instead of piping through sed" and
// Bash's own description already says to prefer Read, Write, Edit, Grep and
// Glob, and session 7a2a2d57 shows a model piping through sed anyway, four
// times in a row. So this is the same bet ToolFloor makes about the
// classifier: for a tool that can run anything, a structural check is worth
// more than a better prompt.
//
// The checks below only fire where the specific tool is a genuine
// replacement. Every case where the shell can do something the tool cannot --
// composing a pipeline, following a growing file, grepping with context lines
// -- is deliberately left alone, because refusing those would cost the model
// an ability rather than redirect it.

// Fallback names the tool that covers a shell command better than the shell
// does, with the reason to hand back to the model.
type Fallback struct {
	Tool   string
	Reason string
}

// deweyTools are the tool names a model sometimes invokes as if they were
// shell programs, having lost track of which calling convention it is in.
var deweyTools = set("Read", "Write", "Edit", "Grep", "Glob")

// contentProducers only exist to emit their arguments, so redirecting one
// into a file is writing that file by hand. Any other program followed by a
// redirect is running something and capturing its output, which is the
// shell's job.
var contentProducers = set("echo", "printf", "cat")

var fileReaders = set("cat", "head", "tail", "less", "more", "bat", "nl")

var contentSearchers = set("grep", "egrep", "fgrep", "rg", "ag", "ack")

// inPlaceFlags mark an editor rewriting its input rather than printing to
// stdout. sed and perl are harmless without them.
var inPlaceFlags = []string{"-i", "--in-place"}

// grepContextFlags ask for surrounding lines, which Grep cannot return.
var grepContextFlags = []string{"-A", "-B", "-C", "--after-context", "--before-context", "--context"}

// findGlobEquivalent are the only find predicates Glob can reproduce.
var findGlobEquivalent = set("-name", "-iname", "-print")

// PreferredTool reports the tool that covers command better than the shell,
// or nil when the shell is the right instrument.
func PreferredTool(command string) *Fallback {
	// Composition is the shell's own capability: no purpose-built tool pipes
	// one program into another or chains on success. A command with more than
	// one segment is therefore always the shell's to run, which is also what
	// keeps `git log | grep fix` from being mistaken for a file search.
	if len(splitCommands(command)) > 1 {
		return nil
	}

	program, args, ok := resolveProgram(command)
	if !ok {
		return nil
	}

	if deweyTools[program] {
		return &Fallback{Tool: program, Reason: program +
			" is a tool, not a shell program. Call it directly instead of through Bash."}
	}

	if redirectsIntoAFile(command) && contentProducers[program] {
		return &Fallback{Tool: "Write", Reason: "this writes a file by redirecting " + program +
			". Use Write to create a file, or Edit to change part of one."}
	}

	if (program == "sed" || program == "perl") && hasAnyFlag(args, inPlaceFlags) {
		return &Fallback{Tool: "Edit", Reason: "this edits a file in place with " + program +
			". Use Edit, which verifies the text it replaces still matches before writing."}
	}
	if program == "patch" || program == "ed" {
		return &Fallback{Tool: "Edit", Reason: "this rewrites a file with " + program +
			". Use Edit for file changes."}
	}

	if fileReaders[program] {
		// -f follows a growing file and Read cannot; a bare reader with no
		// path is consuming stdin; several paths at once is more than one
		// Read call.
		if hasAnyFlag(args, []string{"-f", "--follow"}) {
			return nil
		}
		if paths := nonFlagArgs(args); len(paths) == 1 {
			return &Fallback{Tool: "Read", Reason: "this reads a file with " + program +
				". Use Read, which returns numbered lines and registers the file as read so Edit will accept it."}
		}
		return nil
	}

	if program == "find" || program == "fd" {
		if findIsNameSearchOnly(args) {
			return &Fallback{Tool: "Glob", Reason: "this looks for files by name. Use Glob, " +
				"which takes a pattern like **/*.go directly."}
		}
		return nil
	}

	if contentSearchers[program] {
		if hasAnyFlag(args, grepContextFlags) {
			return nil
		}
		return &Fallback{Tool: "Grep", Reason: "this searches file contents. Use Grep, which " +
			"takes the same regular expression and respects ignore rules."}
	}

	return nil
}

// resolveProgram finds the program a single shell segment actually runs,
// stepping past leading environment assignments and wrappers so
// `FOO=1 env sed -i ...` resolves to sed. Shared with segmentWrites.
func resolveProgram(segment string) (program string, args []string, ok bool) {
	fields := strings.Fields(segment)
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
		return "", nil, false
	}
	return baseName(fields[i]), fields[i+1:], true
}

// redirectsIntoAFile reports whether a segment sends output to a real file,
// as opposed to discarding it or moving a descriptor.
func redirectsIntoAFile(segment string) bool {
	if !writesViaRedirection(segment) {
		return false
	}
	// A redirect to a device sink creates nothing, so it is not a file write.
	for _, field := range strings.Fields(segment) {
		target := strings.TrimLeft(field, "><&12")
		if target != "" && strings.HasPrefix(target, "/dev/") {
			return false
		}
	}
	return true
}

// nonFlagArgs returns the arguments that are not flags, which for a file
// reader are its paths.
func nonFlagArgs(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

// findIsNameSearchOnly reports whether a find invocation does nothing but
// match names, which is the only shape Glob can reproduce. Anything else --
// -exec, -delete, -newer, -type d, a depth bound -- stays with the shell.
func findIsNameSearchOnly(args []string) bool {
	named := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue // a starting path or a predicate's value
		}
		switch {
		case findGlobEquivalent[a]:
			if a != "-print" {
				named = true
				i++ // skip the pattern
			}
		case a == "-type":
			// Glob returns files, so only -type f is equivalent.
			if i+1 >= len(args) || args[i+1] != "f" {
				return false
			}
			i++
		default:
			return false
		}
	}
	return named
}
