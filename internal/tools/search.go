package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
