package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/yargevad/filepathx"
)

// maxSearchResults caps Glob and Grep output. An unbounded file list in a
// monorepo can exceed the whole context window on its own.
const maxSearchResults = 200

type GlobInput struct {
	Pattern string `json:"pattern" jsonschema:"required,description=Glob pattern such as **/*.go or src/**/*.ts"`
	Path    string `json:"path,omitempty" jsonschema:"description=Optional. Absolute directory to search in. Defaults to the working directory"`
}

// searchRoot resolves the directory a search runs in. A relative path would
// resolve against dewey's working directory rather than whatever the model
// had in mind, so it is rejected rather than guessed at.
func searchRoot(path string) (string, error) {
	if path == "" {
		return os.Getwd()
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute, got %q", path)
	}
	return path, nil
}

func globFiles(in GlobInput) (string, error) {
	root, err := searchRoot(in.Path)
	if err != nil {
		return "", err
	}

	matches, err := filepathx.Glob(filepath.Join(root, in.Pattern))
	if err != nil {
		return "", err
	}

	// Most-recently-modified first: when a pattern matches more than fits,
	// the files someone is currently working on are the ones worth showing.
	type entry struct {
		path string
		mod  int64
	}
	entries := make([]entry, 0, len(matches))
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil || info.IsDir() {
			continue
		}
		entries = append(entries, entry{path: m, mod: info.ModTime().UnixNano()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod > entries[j].mod })

	if len(entries) == 0 {
		return fmt.Sprintf("No files matched %q under %s.", in.Pattern, root), nil
	}

	var b strings.Builder
	for i, e := range entries {
		if i >= maxSearchResults {
			fmt.Fprintf(&b, "\n[%d more matches not shown; narrow the pattern]\n", len(entries)-maxSearchResults)
			break
		}
		b.WriteString(e.path)
		b.WriteString("\n")
	}
	return b.String(), nil
}

func newGlobTool() tool.InvokableTool {
	t, _ := utils.InferTool(
		"Glob",
		"Finds files by glob pattern, supporting ** for recursive matching. Returns paths sorted by modification time, newest first. Use this to locate files by name; use Grep to search their contents.",
		func(ctx context.Context, in GlobInput) (string, error) {
			return globFiles(in)
		},
	)
	return t
}

type GrepInput struct {
	Pattern     string `json:"pattern" jsonschema:"required,description=Regular expression to search for"`
	Path        string `json:"path,omitempty" jsonschema:"description=Optional. Absolute directory to search in. Defaults to the working directory"`
	Glob        string `json:"glob,omitempty" jsonschema:"description=Optional. Restrict the search to files matching this glob - supports ** for recursive matching - for example **/*.go"`
	OutputMode  string `json:"output_mode,omitempty" jsonschema:"description=One of content or files_with_matches or count. Defaults to files_with_matches"`
	IgnoreCase  bool   `json:"-i,omitempty" jsonschema:"description=Optional. Case-insensitive search"`
	LineNumbers bool   `json:"-n,omitempty" jsonschema:"description=Optional. Include line numbers in content output"`
}

func (in GrepInput) mode() string {
	switch in.OutputMode {
	case "content", "count", "files_with_matches":
		return in.OutputMode
	default:
		return "files_with_matches"
	}
}

// grepFiles searches with ripgrep when it is available and falls back to a Go
// walk otherwise. Both paths produce path:line:text for content mode so the
// model sees one format regardless of what is installed on the machine.
func grepFiles(in GrepInput) (string, error) {
	// Compile first either way: rg reports a bad pattern as a bare exit code,
	// which is indistinguishable from "no matches" and would surface as a
	// confidently wrong empty result.
	if _, err := regexp.Compile(in.Pattern); err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}
	if _, err := searchRoot(in.Path); err != nil {
		return "", err
	}

	if path, err := exec.LookPath("rg"); err == nil {
		return grepWithRipgrep(path, in)
	}
	return grepWithGo(in)
}

func grepWithRipgrep(rg string, in GrepInput) (string, error) {
	root, err := searchRoot(in.Path)
	if err != nil {
		return "", err
	}

	args := []string{"--color=never"}
	switch in.mode() {
	case "files_with_matches":
		args = append(args, "--files-with-matches")
	case "count":
		args = append(args, "--count")
	default:
		args = append(args, "--no-heading", "--with-filename")
		if in.LineNumbers {
			args = append(args, "--line-number")
		}
	}
	if in.IgnoreCase {
		args = append(args, "--ignore-case")
	}
	if in.Glob != "" {
		args = append(args, "--glob", in.Glob)
	}
	args = append(args, "--regexp", in.Pattern, root)

	out, err := exec.Command(rg, args...).Output()
	// rg exits 1 for "no matches", which is not a failure.
	var exitErr *exec.ExitError
	if err != nil && (!errors.As(err, &exitErr) || exitErr.ExitCode() != 1) {
		return "", err
	}
	return truncateMatches(string(out), in)
}

func grepWithGo(in GrepInput) (string, error) {
	root, err := searchRoot(in.Path)
	if err != nil {
		return "", err
	}

	expr := in.Pattern
	if in.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}

	candidates, err := grepCandidates(root, in.Glob)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	for _, path := range candidates {
		if err := grepOneFile(&b, re, path, in); err != nil {
			return "", err
		}
	}
	return truncateMatches(b.String(), in)
}

// grepCandidates resolves which files to search.
//
// When a glob is given it goes through filepathx, the same ** -capable globber
// the Glob tool uses, because ripgrep's --glob matches whole paths and
// supports **. filepath.Match would match basenames only, so the idiomatic
// "**/*.go" a Claude Code skill writes would return nothing on a machine
// without ripgrep — the two backends have to agree on the file set, not just
// the output format.
func grepCandidates(root, glob string) ([]string, error) {
	if glob != "" {
		matches, err := filepathx.Glob(filepath.Join(root, glob))
		if err != nil {
			return nil, err
		}
		kept := make([]string, 0, len(matches))
		for _, m := range matches {
			info, err := os.Lstat(m)
			if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			kept = append(kept, m)
		}
		return kept, nil
	}

	var paths []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		// ripgrep does not follow symlinks by default, and following one here
		// would read a file outside the root that confinement approved — the
		// check only ever saw the root, not what links inside it point at.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			// .git holds packed binary objects that match almost any pattern.
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	return paths, err
}

// grepOneFile scans a single file and appends whatever the output mode calls
// for. A file that cannot be opened is skipped rather than failing the search.
func grepOneFile(b *strings.Builder, re *regexp.Regexp, path string, in GrepInput) error {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	lineNo, hits := 0, 0
	for scanner.Scan() {
		lineNo++
		if !re.MatchString(scanner.Text()) {
			continue
		}
		hits++
		if in.mode() == "content" {
			if in.LineNumbers {
				fmt.Fprintf(b, "%s:%d:%s\n", path, lineNo, scanner.Text())
			} else {
				fmt.Fprintf(b, "%s:%s\n", path, scanner.Text())
			}
		}
		if b.Len() >= maxToolOutputBytes {
			return nil
		}
	}
	// A line over the scanner's budget would otherwise yield silently partial
	// results, which read as "no more matches".
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(b, "%s: [skipped: %v]\n", path, err)
		return nil
	}

	switch {
	case hits == 0:
	case in.mode() == "files_with_matches":
		fmt.Fprintf(b, "%s\n", path)
	case in.mode() == "count":
		fmt.Fprintf(b, "%s:%s\n", path, strconv.Itoa(hits))
	}
	return nil
}

// truncateMatches caps output and turns an empty result into a statement, so
// "found nothing" is distinguishable from "the call failed".
func truncateMatches(out string, in GrepInput) (string, error) {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return fmt.Sprintf("No matches for %q.", in.Pattern), nil
	}
	if len(out) > maxToolOutputBytes {
		out = out[:maxToolOutputBytes]
		// Drop the partial final line so the output stays parseable.
		if i := strings.LastIndexByte(out, '\n'); i > 0 {
			out = out[:i]
		}
		return out + fmt.Sprintf("\n[truncated at %d bytes; narrow the pattern]", maxToolOutputBytes), nil
	}
	lines := strings.Split(out, "\n")
	if len(lines) > maxSearchResults {
		return strings.Join(lines[:maxSearchResults], "\n") +
			fmt.Sprintf("\n[%d more lines not shown; narrow the pattern or use output_mode=files_with_matches]", len(lines)-maxSearchResults), nil
	}
	return out + "\n", nil
}

func newGrepTool() tool.InvokableTool {
	t, _ := utils.InferTool(
		"Grep",
		"Searches file contents with a regular expression. output_mode selects content (matching lines), files_with_matches (paths only, the default), or count. Supports -i for case-insensitive and -n for line numbers. Context lines are not supported; use Read with an offset instead.",
		func(ctx context.Context, in GrepInput) (string, error) {
			return grepFiles(in)
		},
	)
	return t
}
