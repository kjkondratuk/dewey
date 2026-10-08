// Package herdr reports dewey's state to Herdr (https://herdr.dev) when
// dewey is running inside a Herdr pane, per
// https://herdr.dev/docs/add-herdr-support/.
//
// Outside Herdr, a Reporter is a no-op: every method returns immediately
// without touching the filesystem or spawning a process, so call sites never
// need to check whether Herdr is present themselves.
package herdr

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// timeout bounds every call out to the Herdr binary, so a hung or missing
// binary never stalls the agent. Reports are best-effort: failures, including
// a timeout, are ignored.
const timeout = 3 * time.Second

// Reporter reports dewey's agent/idle/blocked state to a Herdr pane.
type Reporter struct {
	enabled bool
	binPath string
	paneID  string
	agent   string

	lastSeq atomic.Int64

	mu         sync.Mutex
	sessionID  string
	resumeArgv []string
	pending    *report
	draining   bool
}

type report struct {
	state   string
	message string
	seq     int64
}

// New builds a Reporter identifying itself to Herdr as agent/source name
// agent, reading the HERDR_* environment variables every process in a Herdr
// pane inherits. When HERDR_ENV, HERDR_BIN_PATH, or HERDR_PANE_ID is absent,
// the returned Reporter is disabled and every method is a no-op -- per the
// docs, dewey must do nothing outside Herdr rather than guess at defaults.
func New(agent string) *Reporter {
	r := &Reporter{agent: agent}
	if os.Getenv("HERDR_ENV") != "1" {
		return r
	}
	binPath := os.Getenv("HERDR_BIN_PATH")
	paneID := os.Getenv("HERDR_PANE_ID")
	if binPath == "" || paneID == "" {
		return r
	}
	r.enabled = true
	r.binPath = binPath
	r.paneID = paneID
	return r
}

// SetSession records how Herdr should resume this run: the session id dewey
// is recording to (shown to the user via --sessions) and the argv that
// restarts it, e.g. []string{"dewey", "--resume", id}. It is attached to
// every subsequent state report, not just the next one, since reports can be
// dropped (see ReportState) and the resume command must still reach Herdr
// even if the report it first rode along with did not.
func (r *Reporter) SetSession(sessionID string, resumeArgv []string) {
	if r == nil || !r.enabled {
		return
	}
	r.mu.Lock()
	r.sessionID = sessionID
	r.resumeArgv = resumeArgv
	r.mu.Unlock()
}

// ReportState reports dewey's state as "working", "idle", or "blocked", with
// message explaining why when blocked (ignored for the other two states).
//
// It returns immediately. The actual report runs in the background with a
// short timeout, and only the most recent of several states queued while one
// is still in flight is ever sent -- an idle->working->idle flicker inside
// one send's timeout reports just the final idle, per the docs' guidance to
// drop superseded reports rather than fire them all.
func (r *Reporter) ReportState(state, message string) {
	if r == nil || !r.enabled {
		return
	}
	rep := report{state: state, message: message, seq: r.nextSeq()}

	r.mu.Lock()
	r.pending = &rep
	alreadyDraining := r.draining
	r.draining = true
	r.mu.Unlock()

	if !alreadyDraining {
		go r.drain()
	}
}

// drain sends whatever is pending until nothing is, so a burst of
// ReportState calls results in one goroutine and one send per surviving
// state rather than one of each per call.
func (r *Reporter) drain() {
	for {
		r.mu.Lock()
		rep := r.pending
		r.pending = nil
		if rep == nil {
			r.draining = false
			r.mu.Unlock()
			return
		}
		sessionID, resumeArgv := r.sessionID, r.resumeArgv
		r.mu.Unlock()

		r.send(*rep, sessionID, resumeArgv)
	}
}

// Release tells Herdr dewey has quit and the pane is free. Unlike
// ReportState, it runs synchronously -- still bounded by the same timeout --
// so it has a real chance to land before the process exits. Call it last,
// after the TUI has returned.
func (r *Reporter) Release() {
	if r == nil || !r.enabled {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := []string{
		"pane", "release-agent", r.paneID,
		"--source", r.agent,
		"--agent", r.agent,
		"--seq", strconv.FormatInt(r.nextSeq(), 10),
	}
	_ = exec.CommandContext(ctx, r.binPath, args...).Run()
}

func (r *Reporter) send(rep report, sessionID string, resumeArgv []string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := []string{
		"pane", "report-agent", r.paneID,
		"--source", r.agent,
		"--agent", r.agent,
		"--state", rep.state,
		"--seq", strconv.FormatInt(rep.seq, 10),
	}
	if rep.message != "" {
		args = append(args, "--message", rep.message)
	}
	if sessionID != "" {
		args = append(args, "--agent-session-id", sessionID)
	}
	if len(resumeArgv) > 0 {
		args = append(args, "--")
		args = append(args, resumeArgv...)
	}
	_ = exec.CommandContext(ctx, r.binPath, args...).Run()
}

// nextSeq returns a strictly increasing sequence number, even across
// restarts within the same second: Herdr drops any report whose seq is not
// numerically newer than the last one it saw for this source, so two calls
// landing in the same nanosecond (unlikely but not impossible) must not
// produce the same value.
func (r *Reporter) nextSeq() int64 {
	now := time.Now().UnixNano()
	for {
		last := r.lastSeq.Load()
		next := now
		if next <= last {
			next = last + 1
		}
		if r.lastSeq.CompareAndSwap(last, next) {
			return next
		}
	}
}

// FirstLine returns the first line of s, for use as a Herdr --message: the
// docs ask for a short explanation of a blocked state, and dewey's
// confirmation prompts are multi-line markdown with the summary worth
// surfacing on the first line already.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
