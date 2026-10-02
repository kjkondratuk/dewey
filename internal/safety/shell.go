package safety

import (
	"encoding/json"
	"strings"
)

// mutatingCommands are programs whose ordinary use changes state. The list is
// deliberately not exhaustive — it cannot be — and exists only to establish a
// floor the classifier may raise but never lower.
var mutatingCommands = map[string]bool{
	// filesystem
	"rm": true, "rmdir": true, "mv": true, "cp": true, "touch": true,
	"mkdir": true, "ln": true, "tee": true, "dd": true, "truncate": true,
	"shred": true, "install": true, "rsync": true, "chmod": true,
	"chown": true, "chgrp": true, "chflags": true, "unlink": true,
	// editors and in-place rewriters
	"vi": true, "vim": true, "nano": true, "emacs": true, "patch": true,
	// archives that extract or create
	"tar": true, "unzip": true, "zip": true, "gunzip": true, "gzip": true,
	// version control that mutates
	"git": true, "svn": true, "hg": true,
	// package and build tooling
	"npm": true, "yarn": true, "pnpm": true, "pip": true, "pip3": true,
	"gem": true, "cargo": true, "go": true, "brew": true, "apt": true,
	"apt-get": true, "yum": true, "dnf": true, "make": true, "mise": true,
	// containers and services
	"docker": true, "podman": true, "kubectl": true, "helm": true,
	"systemctl": true, "launchctl": true, "service": true,
	// processes and system state
	"kill": true, "killall": true, "pkill": true, "sudo": true, "su": true,
	"reboot": true, "shutdown": true, "mount": true, "umount": true,
	"defaults": true, "crontab": true,
	// network fetchers that can write files
	"curl": true, "wget": true, "scp": true, "sftp": true,
}

// commandSeparators split a shell line into separately-executed commands. A
// mutating program anywhere in the pipeline counts, not just at the start.
var commandSeparators = []string{"&&", "||", ";", "|", "\n"}

// ShellFloor returns the minimum classification for a shell command: a
// Classification whose Kind is "write" when the command obviously changes
// state, or the zero value when nothing was recognized.
//
// This exists because the confirmation gate must not depend solely on a local
// model's judgement. The classifier is a language model asked to reason about
// consequences, and it demonstrably rates `echo hello > f.txt` as "safe". For
// a tool that can run anything, a structural check that the classifier may
// raise but never lower is worth more than a better prompt.
//
// It is a floor, not a parser and not a security boundary. It does not expand
// variables, follow aliases, read scripts being invoked, or understand
// `find -exec`. Anything it misses still goes to the classifier. False
// positives cost an extra confirmation, which is the right direction to err.
func ShellFloor(argsJSON string) Classification {
	command, ok := shellCommandFrom(argsJSON)
	if !ok {
		return Classification{}
	}

	for _, segment := range splitCommands(command) {
		if writesViaRedirection(segment) || invokesMutatingProgram(segment) {
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
// Descriptor duplications like 2>&1 move an existing stream and are stripped
// first so they don't read as writes.
func writesViaRedirection(segment string) bool {
	cleaned := segment
	for _, dup := range []string{"2>&1", "1>&2", ">&1", ">&2", "2>&-", ">&-"} {
		cleaned = strings.ReplaceAll(cleaned, dup, "")
	}
	return strings.Contains(cleaned, ">")
}

func invokesMutatingProgram(segment string) bool {
	fields := strings.Fields(segment)
	for _, field := range fields {
		// Skip env assignments and leading env/command wrappers so
		// `FOO=1 env rm x` is still recognized.
		if strings.Contains(field, "=") || field == "env" || field == "command" ||
			field == "exec" || field == "time" || field == "nohup" {
			continue
		}
		// A path-qualified program still counts: /bin/rm is rm.
		name := field
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		return mutatingCommands[name]
	}
	return false
}
