// Package prompts holds every piece of model-facing text dewey sends, kept as
// markdown files so they can be read and edited as prose instead of as Go
// string literals. The files are compiled into the binary, so a built dewey
// has no runtime dependency on this directory.
//
// Each exported value is trimmed of surrounding whitespace: the files end with
// a trailing newline the way text files should, and the models shouldn't see
// it.
package prompts

import (
	_ "embed"
	"strings"
)

//go:embed classifier.md
var classifierMD string

//go:embed summarizer.md
var summarizerMD string

//go:embed tool_denied.md
var toolDeniedMD string

//go:embed system_base.md
var systemBaseMD string

//go:embed system_memory.md
var systemMemoryMD string

var (
	// Classifier is the system prompt for the tool-call safety classifier.
	// It must keep instructing the model to answer with a bare JSON object —
	// internal/safety parses the response and fails safe if it can't.
	Classifier = strings.TrimSpace(classifierMD)

	// Summarizer is the system prompt used to condense conversation history
	// when the context window fills up.
	Summarizer = strings.TrimSpace(summarizerMD)

	// ToolDenied is fed back to the model as the tool result when the user
	// declines a tool call. It is not a system prompt, but it is model-facing
	// instruction text with the same reasons to live outside the Go source:
	// its exact wording is what stops a denial being read as a missing
	// capability.
	ToolDenied = strings.TrimSpace(toolDeniedMD)

	// SystemBase is the base system prompt: how dewey behaves before any of
	// the user's own remembered preferences are layered on.
	SystemBase = strings.TrimSpace(systemBaseMD)

	// SystemMemory explains the remember tool. It is a separate fragment
	// because it must only be sent when memory is actually enabled — telling
	// the model about a tool that was never bound invites it to promise
	// something it cannot do, or to call a tool that does not exist.
	SystemMemory = strings.TrimSpace(systemMemoryMD)
)
