package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/kjkondratuk/dewey/internal/files"
)

// defaultReadLimit caps an unbounded Read. Matches Claude Code's default, so
// skills that assume "Read gives me the first 2000 lines" hold.
const defaultReadLimit = 2000

type ReadInput struct {
	FilePath string `json:"file_path" jsonschema:"required,description=Absolute path to the file to read"`
	Offset   int    `json:"offset,omitempty" jsonschema:"description=Optional. 1-indexed line number to start from. Omit to start at the beginning"`
	Limit    int    `json:"limit,omitempty" jsonschema:"description=Optional. Maximum number of lines to return. Omit for 2000"`
}

// readFile is the tool body, separated from registration so it can be tested
// without going through JSON schema inference.
func readFile(tracker *files.Tracker, in ReadInput) (string, error) {
	if !filepath.IsAbs(in.FilePath) {
		return "", fmt.Errorf("file_path must be absolute, got %q", in.FilePath)
	}

	f, err := os.Open(in.FilePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory; use Glob or Bash to list it", in.FilePath)
	}

	offset := in.Offset
	if offset < 1 {
		offset = 1
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}

	var b strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	lineNo, emitted := 0, 0
	for scanner.Scan() {
		lineNo++
		if lineNo < offset {
			continue
		}
		if emitted >= limit {
			fmt.Fprintf(&b, "\n[truncated at %d lines; re-read with offset=%d for more]\n", limit, lineNo)
			break
		}
		fmt.Fprintf(&b, "%6d\t%s\n", lineNo, scanner.Text())
		emitted++
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// Record the read before returning: this is what lets Write and Edit touch
	// the file, and an unrecorded successful read would block them for no
	// reason the model could diagnose.
	tracker.MarkRead(in.FilePath, info.ModTime())

	if emitted == 0 {
		if lineNo == 0 {
			return fmt.Sprintf("%s is empty.", in.FilePath), nil
		}
		return fmt.Sprintf("%s has %d lines; offset %d is past the end.", in.FilePath, lineNo, offset), nil
	}
	return b.String(), nil
}

func newReadTool(tracker *files.Tracker) tool.InvokableTool {
	t, _ := utils.InferTool(
		"Read",
		"Reads a file from the filesystem and returns its contents with line numbers. Reads up to 2000 lines by default; use offset and limit for a specific range. Must be called before Write or Edit can modify an existing file.",
		func(ctx context.Context, in ReadInput) (string, error) {
			return readFile(tracker, in)
		},
	)
	return t
}
