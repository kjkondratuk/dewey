package herdr

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHerdrBin writes a shell script standing in for the Herdr binary: each
// invocation appends its args, newline-joined, as one line to the log file.
// Using a real subprocess (rather than faking exec.Command) exercises the
// actual argv construction, including the "--" separator before a resume
// command.
func fakeHerdrBin(t *testing.T) (binPath, logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a shell script")
	}
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	binPath = filepath.Join(dir, "herdr")
	script := "#!/bin/sh\necho \"$@\" >> \"" + logPath + "\"\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake binary: %v", err)
	}
	return binPath, logPath
}

func readLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// waitForSends blocks until n calls to the Herdr binary have completed,
// confirmed via r.sent rather than by polling the log file against a guessed
// wall-clock deadline. The 5s cap only guards against a genuine hang -- it is
// not what normally ends the wait.
func waitForSends(t *testing.T, sent <-chan struct{}, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-sent:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for send %d/%d to complete", i+1, n)
		}
	}
}

func newEnabledReporter(t *testing.T, agent string) (*Reporter, string, chan struct{}) {
	t.Helper()
	bin, log := fakeHerdrBin(t)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_BIN_PATH", bin)
	t.Setenv("HERDR_PANE_ID", "pane-123")
	r := New(agent)
	sent := make(chan struct{}, 8)
	r.sent = sent
	return r, log, sent
}

func TestNew_DisabledOutsideHerdr(t *testing.T) {
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_BIN_PATH", "/bin/true")
	t.Setenv("HERDR_PANE_ID", "pane-123")
	r := New("dewey")
	if r.enabled {
		t.Fatal("expected a disabled Reporter when HERDR_ENV is unset")
	}
}

func TestNew_DisabledWithoutPaneOrBinPath(t *testing.T) {
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_BIN_PATH", "")
	t.Setenv("HERDR_PANE_ID", "pane-123")
	if New("dewey").enabled {
		t.Fatal("expected disabled Reporter with no HERDR_BIN_PATH")
	}

	t.Setenv("HERDR_BIN_PATH", "/bin/true")
	t.Setenv("HERDR_PANE_ID", "")
	if New("dewey").enabled {
		t.Fatal("expected disabled Reporter with no HERDR_PANE_ID")
	}
}

func TestReportState_NoopMethodsOnNilReporter(t *testing.T) {
	var r *Reporter
	// Must not panic -- app.go calls these unconditionally, and test
	// fixtures construct an App with no Herdr set.
	r.ReportState("working", "")
	r.SetSession("abc", []string{"dewey", "--resume", "abc"})
	r.Release()
}

func TestReportState_DisabledIsNoop(t *testing.T) {
	bin, log := fakeHerdrBin(t)
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_BIN_PATH", bin)
	t.Setenv("HERDR_PANE_ID", "pane-123")
	r := New("dewey")
	r.ReportState("working", "")
	r.Release()
	time.Sleep(50 * time.Millisecond)
	if lines := readLog(t, log); len(lines) != 0 {
		t.Fatalf("expected no calls to the Herdr binary, got %v", lines)
	}
}

func TestReportState_SendsExpectedArgs(t *testing.T) {
	r, log, sent := newEnabledReporter(t, "dewey")
	r.ReportState("working", "")
	waitForSends(t, sent, 1)

	lines := readLog(t, log)
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d: %v", len(lines), lines)
	}
	fields := strings.Fields(lines[0])
	want := []string{"pane", "report-agent", "pane-123", "--source", "dewey", "--agent", "dewey", "--state", "working"}
	for i, w := range want {
		if fields[i] != w {
			t.Fatalf("arg %d: got %q, want %q (full: %v)", i, fields[i], w, fields)
		}
	}
}

func TestReportState_BlockedIncludesMessage(t *testing.T) {
	r, log, sent := newEnabledReporter(t, "dewey")
	r.ReportState("blocked", "Bash wants to run (WRITE) [risk: dangerous]")
	waitForSends(t, sent, 1)

	lines := readLog(t, log)
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "--message Bash wants to run (WRITE) [risk: dangerous]") {
		t.Fatalf("expected a --message arg, got: %q", lines[0])
	}
}

func TestSetSession_AttachesSessionAndResumeArgv(t *testing.T) {
	r, log, sent := newEnabledReporter(t, "dewey")
	r.SetSession("sess-1", []string{"dewey", "--resume", "sess-1"})
	r.ReportState("idle", "")
	waitForSends(t, sent, 1)

	lines := readLog(t, log)
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "--agent-session-id sess-1") {
		t.Fatalf("expected --agent-session-id, got: %q", lines[0])
	}
	if !strings.HasSuffix(lines[0], "-- dewey --resume sess-1") {
		t.Fatalf("expected a trailing resume argv after --, got: %q", lines[0])
	}
}

// TestReportState_DropsSupersededReports exercises the "only the latest
// queued report is sent" requirement: block the fake binary on the first
// call so a burst of further ReportState calls queues up behind it, then
// confirm only the final state is ever sent.
func TestReportState_DropsSupersededReports(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a shell script")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	gatePath := filepath.Join(dir, "gate")
	binPath := filepath.Join(dir, "herdr")
	// The first call waits for gatePath to exist before logging and
	// returning, holding drain() inside send() so every ReportState call
	// made in the meantime collapses into the single still-pending slot.
	script := "#!/bin/sh\n" +
		"if [ ! -f \"" + gatePath + "\" ]; then\n" +
		"  while [ ! -f \"" + gatePath + "\" ]; do sleep 0.01; done\n" +
		"fi\n" +
		"echo \"$@\" >> \"" + logPath + "\"\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake binary: %v", err)
	}

	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_BIN_PATH", binPath)
	t.Setenv("HERDR_PANE_ID", "pane-123")
	r := New("dewey")
	sent := make(chan struct{}, 8)
	r.sent = sent

	r.ReportState("working", "")
	// Give drain() time to start the first (gated) send before queuing more.
	time.Sleep(50 * time.Millisecond)
	r.ReportState("blocked", "a")
	r.ReportState("working", "")
	r.ReportState("idle", "")

	if err := os.WriteFile(gatePath, nil, 0o644); err != nil {
		t.Fatalf("opening gate: %v", err)
	}

	waitForSends(t, sent, 2)
	lines := readLog(t, logPath)
	if len(lines) != 2 {
		t.Fatalf("expected exactly 2 sends (first + collapsed latest), got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[1], "--state idle") {
		t.Fatalf("expected the second send to be the final 'idle' state, got: %q", lines[1])
	}
}

func TestRelease_SendsExpectedArgs(t *testing.T) {
	r, log, sent := newEnabledReporter(t, "dewey")
	r.Release()
	waitForSends(t, sent, 1)

	lines := readLog(t, log)
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d: %v", len(lines), lines)
	}
	fields := strings.Fields(lines[0])
	want := []string{"pane", "release-agent", "pane-123", "--source", "dewey", "--agent", "dewey"}
	for i, w := range want {
		if fields[i] != w {
			t.Fatalf("arg %d: got %q, want %q (full: %v)", i, fields[i], w, fields)
		}
	}
}

// TestNextSeq_SequentialCallsStrictlyIncrease checks the property one caller
// actually relies on: calls made one after another (as every call site in
// this package does, each under r.mu) never repeat or go backward, even
// when several land in the same nanosecond.
func TestNextSeq_SequentialCallsStrictlyIncrease(t *testing.T) {
	r := &Reporter{}
	last := r.nextSeq()
	for i := 0; i < 10000; i++ {
		seq := r.nextSeq()
		if seq <= last {
			t.Fatalf("seq %d not strictly greater than previous %d", seq, last)
		}
		last = seq
	}
}

// TestNextSeq_ConcurrentCallsNeverRepeat checks the weaker property that
// holds under concurrency: nextSeq has no synchronization of its own
// (callers serialize through r.mu before using its result), so concurrent
// calls can only be guaranteed not to collide, not to complete in the order
// they were issued.
func TestNextSeq_ConcurrentCallsNeverRepeat(t *testing.T) {
	r := &Reporter{}
	var (
		mu   sync.Mutex
		seen = map[int64]bool{}
	)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				seq := r.nextSeq()
				mu.Lock()
				if seen[seq] {
					t.Errorf("seq %d produced twice", seq)
				}
				seen[seq] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}

func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"Bash wants to run (WRITE) [risk: dangerous]\n\nDeletes a file.\n\n```\nrm -rf ./dist\n```\nAllow? [y/n]": "Bash wants to run (WRITE) [risk: dangerous]",
		"single line":      "single line",
		"  padded \n more": "padded",
		"":                 "",
	}
	for in, want := range cases {
		if got := FirstLine(in); got != want {
			t.Errorf("FirstLine(%q) = %q, want %q", in, got, want)
		}
	}
}
