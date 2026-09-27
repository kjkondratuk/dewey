// Package memory persists the user's preferences and durable facts across
// projects and sessions, so dewey starts a session already knowing how this
// person works rather than rediscovering it every time.
package memory

import "time"

// Memory is one remembered fact or preference.
type Memory struct {
	// ID is a stable slug, also the filename stem. Used for dedup and so a
	// memory can be referred to by hand.
	ID string

	// Text is the memory itself, in plain prose.
	Text string

	// Org gates the memory: empty means it applies anywhere, non-empty means
	// it applies only when the detected org matches exactly. This is
	// deliberately stricter than Tags — see Context.
	Org string

	// Tags are topical (stack, kind of work). Empty means no topical
	// restriction; otherwise at least one must match the context.
	Tags []string

	Created time.Time
}

// Context is what the current working directory implies about the work being
// done. Org is matched exactly and gates; Tags are matched by intersection.
type Context struct {
	Org  string
	Tags []string
}
