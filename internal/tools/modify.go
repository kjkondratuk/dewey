package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/kjkondratuk/dewey/internal/files"
)

type WriteInput struct {
	FilePath string `json:"file_path" jsonschema:"required,description=Absolute path to the file to write"`
	Content  string `json:"content" jsonschema:"required,description=The complete contents to write. Replaces the file entirely"`
}

type EditInput struct {
	FilePath   string `json:"file_path" jsonschema:"required,description=Absolute path to the file to edit"`
	OldString  string `json:"old_string" jsonschema:"required,description=The exact text to replace. Must appear exactly once unless replace_all is true"`
	NewString  string `json:"new_string" jsonschema:"required,description=The text to replace it with. Must differ from old_string"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"description=Optional. Replace every occurrence instead of requiring exactly one"`
}

// persist writes content to path and re-records the read, so a following
// modification is not rejected for the mtime this write just changed.
func persist(tracker *files.Tracker, path, content string, perm os.FileMode) error {
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tracker.MarkRead(path, info.ModTime())
	return nil
}

func writeFile(tracker *files.Tracker, in WriteInput) (string, error) {
	if !filepath.IsAbs(in.FilePath) {
		return "", fmt.Errorf("file_path must be absolute, got %q", in.FilePath)
	}
	if err := tracker.CheckWritable(in.FilePath); err != nil {
		return "", err
	}

	// Preserve an existing file's mode: rewriting a script must not strip its
	// executable bit.
	perm := os.FileMode(0o644)
	if info, err := os.Stat(in.FilePath); err == nil {
		perm = info.Mode().Perm()
	}

	if err := persist(tracker, in.FilePath, in.Content, perm); err != nil {
		return "", err
	}
	return fmt.Sprintf("Wrote %d bytes to %s.", len(in.Content), in.FilePath), nil
}

func editFile(tracker *files.Tracker, in EditInput) (string, error) {
	if !filepath.IsAbs(in.FilePath) {
		return "", fmt.Errorf("file_path must be absolute, got %q", in.FilePath)
	}
	if in.OldString == in.NewString {
		return "", fmt.Errorf("old_string and new_string are identical; nothing to change")
	}
	if err := tracker.CheckWritable(in.FilePath); err != nil {
		return "", err
	}

	info, err := os.Stat(in.FilePath)
	if err != nil {
		return "", err
	}
	original, err := os.ReadFile(in.FilePath)
	if err != nil {
		return "", err
	}
	text := string(original)

	count := strings.Count(text, in.OldString)
	switch {
	case count == 0:
		return "", fmt.Errorf("old_string not found in %s; read the file again to see its current contents", in.FilePath)
	case count > 1 && !in.ReplaceAll:
		// Refusing is the whole point: replacing the first of several matches
		// edits a line the model did not choose, and nothing downstream can
		// detect that it happened.
		return "", fmt.Errorf("old_string appears %d times in %s; make it unique or set replace_all", count, in.FilePath)
	}

	updated := text
	replaced := count
	if in.ReplaceAll {
		updated = strings.ReplaceAll(text, in.OldString, in.NewString)
	} else {
		updated = strings.Replace(text, in.OldString, in.NewString, 1)
		replaced = 1
	}

	if err := persist(tracker, in.FilePath, updated, info.Mode().Perm()); err != nil {
		return "", err
	}
	return fmt.Sprintf("Replaced %d occurrence(s) in %s.", replaced, in.FilePath), nil
}

func newWriteTool(tracker *files.Tracker) tool.InvokableTool {
	t, _ := utils.InferTool(
		"Write",
		"Writes a file to the filesystem, replacing it entirely if it exists. An existing file must be read first. For changing part of a file, prefer Edit.",
		func(ctx context.Context, in WriteInput) (string, error) {
			return writeFile(tracker, in)
		},
	)
	return t
}

func newEditTool(tracker *files.Tracker) tool.InvokableTool {
	t, _ := utils.InferTool(
		"Edit",
		"Performs an exact string replacement in a file. The file must be read first. old_string must appear exactly once unless replace_all is set, so an ambiguous edit fails instead of changing the wrong line.",
		func(ctx context.Context, in EditInput) (string, error) {
			return editFile(tracker, in)
		},
	)
	return t
}
