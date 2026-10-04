package safety

import (
	_ "embed"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed default_rules.yaml
var defaultRulesYAML []byte

// ProgramRules describes how one program is classified when it both reads and
// writes depending on how it is called.
type ProgramRules struct {
	// Subcommands that only observe. Listing a program at all means an
	// unrecognized invocation of it counts as a write, so a subcommand nobody
	// anticipated errs toward prompting rather than running silently.
	Subcommands []string `yaml:"subcommands"`

	// Patterns matching read-only invocations that a subcommand list cannot
	// express — `git branch` listing versus `git branch -d old` deleting. Each
	// is matched against the arguments after the program name.
	//
	// Patterns describe *reads*, never writes. A pattern language for writes
	// would mean anything unmatched falls through as a read, which turns an
	// unanticipated command into a silent write — the one direction this
	// cannot afford.
	Patterns []string `yaml:"patterns"`
}

// ShellRules is the deterministic floor applied to shell commands before the
// classifier's answer is used.
type ShellRules struct {
	// AlwaysWrite are programs whose purpose is changing something; no
	// argument inspection makes them read-only.
	AlwaysWrite []string `yaml:"always_write"`

	// NeverWrite removes programs from AlwaysWrite. This is the one knob that
	// weakens the floor, so it is spelled out separately rather than being an
	// absence somewhere — and dewey reports it at startup.
	NeverWrite []string `yaml:"never_write"`

	// ReadsWhen holds the subcommand- and pattern-based programs.
	ReadsWhen map[string]ProgramRules `yaml:"reads_when"`

	// WritesWithFlags are programs that read by default and write only when
	// one of the listed flags appears.
	WritesWithFlags map[string][]string `yaml:"writes_with_flags"`

	// AlwaysWriteTools are non-shell tools that write by definition. The
	// remember tool writes to the user's profile whatever a classifier thinks.
	AlwaysWriteTools []string `yaml:"always_write_tools"`

	// AlwaysReadTools are tools whose implementation cannot modify anything,
	// so the classifier is skipped for them entirely.
	//
	// This is the only rule that lowers a classification rather than raising
	// it, which is defensible only because it describes dewey's own code: Read
	// opens a file, Glob matches names, Grep searches text, and none of them
	// has a write path. It is not a judgement about arguments, so there is
	// nothing for an argument to smuggle past. AlwaysRead refuses to apply it
	// to the shell tool or to anything also listed as always-write, so a
	// mistaken config cannot turn it into a hole in the approval gate.
	AlwaysReadTools []string `yaml:"always_read_tools"`

	// compiled holds the parsed Patterns, built once by Compile.
	compiled map[string][]*regexp.Regexp
}

// DefaultShellRules returns the built-in rules covering common OS commands and
// developer tooling. They are data, not code, so a project can extend them
// without rebuilding dewey.
func DefaultShellRules() (ShellRules, error) {
	var r ShellRules
	if err := yaml.Unmarshal(defaultRulesYAML, &r); err != nil {
		return ShellRules{}, fmt.Errorf("parsing built-in shell rules: %w", err)
	}
	return r, r.Compile()
}

// Compile parses the pattern strings. Call it after any merge; an invalid
// pattern is an error rather than a silently-skipped rule, because a rule the
// user believes is in effect is exactly the failure this package exists to
// avoid.
func (r *ShellRules) Compile() error {
	r.compiled = make(map[string][]*regexp.Regexp, len(r.ReadsWhen))
	programs := make([]string, 0, len(r.ReadsWhen))
	for program := range r.ReadsWhen {
		programs = append(programs, program)
	}
	sort.Strings(programs) // deterministic error ordering

	for _, program := range programs {
		for _, pattern := range r.ReadsWhen[program].Patterns {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return fmt.Errorf("shell rule for %q: invalid pattern %q: %w", program, pattern, err)
			}
			r.compiled[program] = append(r.compiled[program], re)
		}
	}
	return nil
}

// Merge layers user rules over these, returning the result and a list of
// notices describing anything the user weakened.
//
// Additions are the common case and merge quietly. Removals are reported:
// taking `sudo` out of AlwaysWrite is a legitimate thing to want and a thing
// the user should be reminded they did.
func (r ShellRules) Merge(user ShellRules) (ShellRules, []string) {
	var notices []string

	merged := ShellRules{
		AlwaysWrite:      append(append([]string(nil), r.AlwaysWrite...), user.AlwaysWrite...),
		AlwaysWriteTools: append(append([]string(nil), r.AlwaysWriteTools...), user.AlwaysWriteTools...),
		AlwaysReadTools:  append(append([]string(nil), r.AlwaysReadTools...), user.AlwaysReadTools...),
		ReadsWhen:        make(map[string]ProgramRules, len(r.ReadsWhen)),
		WritesWithFlags:  make(map[string][]string, len(r.WritesWithFlags)),
	}

	if len(user.NeverWrite) > 0 {
		remove := set(user.NeverWrite...)
		var kept []string
		var dropped []string
		for _, program := range merged.AlwaysWrite {
			if remove[program] {
				dropped = append(dropped, program)
				continue
			}
			kept = append(kept, program)
		}
		merged.AlwaysWrite = kept
		if len(dropped) > 0 {
			notices = append(notices, fmt.Sprintf(
				"shell rules: %s no longer always require approval (never_write)",
				strings.Join(uniqueSorted(dropped), ", ")))
		}
	}

	// Read rules merge per program so adding one subcommand doesn't discard
	// the built-ins for that program.
	for program, rules := range r.ReadsWhen {
		merged.ReadsWhen[program] = rules
	}
	for program, rules := range user.ReadsWhen {
		base := merged.ReadsWhen[program]
		merged.ReadsWhen[program] = ProgramRules{
			Subcommands: append(append([]string(nil), base.Subcommands...), rules.Subcommands...),
			Patterns:    append(append([]string(nil), base.Patterns...), rules.Patterns...),
		}
		if len(rules.Subcommands) > 0 || len(rules.Patterns) > 0 {
			if _, builtin := r.ReadsWhen[program]; !builtin {
				continue // a program the defaults never covered; nothing weakened
			}
			notices = append(notices, fmt.Sprintf(
				"shell rules: %s has additional read-only invocations from config", program))
		}
	}

	// Flag lists replace wholesale: they are short, and an append would make
	// removing a wrong default impossible.
	for program, flags := range r.WritesWithFlags {
		merged.WritesWithFlags[program] = flags
	}
	for program, flags := range user.WritesWithFlags {
		if _, builtin := r.WritesWithFlags[program]; builtin {
			notices = append(notices, fmt.Sprintf(
				"shell rules: write-flag list for %s replaced by config", program))
		}
		merged.WritesWithFlags[program] = flags
	}

	// An always-read entry that names the shell tool or a tool already pinned
	// to write is ignored rather than honored. Silently dropping it would
	// leave the user believing a rule is in effect, which is the failure this
	// package exists to avoid.
	//
	// set() is an existing helper in this package (see the NeverWrite handling
	// above); it is not introduced here.
	writeTools := set(merged.AlwaysWriteTools...)
	var keptReads []string
	for _, name := range merged.AlwaysReadTools {
		switch {
		case name == shellToolName:
			notices = append(notices, fmt.Sprintf(
				"ignoring always_read_tools entry %q: the shell tool is never exempt from classification", name))
		case writeTools[name]:
			notices = append(notices, fmt.Sprintf(
				"ignoring always_read_tools entry %q: it is also listed in always_write_tools", name))
		default:
			keptReads = append(keptReads, name)
		}
	}
	merged.AlwaysReadTools = keptReads

	return merged, notices
}

func uniqueSorted(values []string) []string {
	seen := set(values...)
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// contains reports whether values includes want.
func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func set(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}
