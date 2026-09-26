package agent

import "context"

type Event struct {
	Kind    string
	Payload string

	// Confirm is set only when Kind == "confirm". The receiver must send
	// exactly one value on it to unblock the agent: true to allow the
	// pending tool call, false to deny it. Leaving it unanswered and
	// cancelling the context instead takes the cancellation path.
	Confirm chan<- bool
}

type Agent interface {
	Run(ctx context.Context, input string) (<-chan Event, error)
}
