package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjkondratuk/dewey/internal/files"
)

func lookRg() (string, error) { return exec.LookPath("rg") }

// Grep's Go backend follows a symlink pointing out of the search root.
//
// The assertion checks for the symlinked file's own name rather than the
// secret content: truncateMatches' "No matches for %q" message echoes the
// search pattern itself, and this test's pattern happens to equal the secret
// content, so asserting on the raw secret string is a false positive on a
// correct "no matches" result. Checking for the leaking file's name has no
// such collision.
func TestAdversarial_GrepWithGo_FollowsSymlinkOutOfRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(parent, "id_rsa")
	if err := os.WriteFile(secret, []byte("SECRETKEYMATERIAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "notes.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	got, err := grepWithGo(GrepInput{Pattern: "SECRETKEYMATERIAL", Path: root, OutputMode: "content"})
	if err != nil {
		t.Fatalf("grepWithGo() error = %v", err)
	}
	if strings.Contains(got, "notes.txt") {
		t.Errorf("grepWithGo over %s returned content from %s via a symlink\ngot:\n%s", root, secret, got)
	}
}

// Edit's uniqueness guard misses overlapping occurrences.
func TestAdversarial_Edit_OverlappingOldStringBypassesUniquenessGuard(t *testing.T) {
	tracker := files.NewTracker()
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("ababa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(tracker, ReadInput{FilePath: path}); err != nil {
		t.Fatal(err)
	}

	// "aba" occurs at offsets 0 and 2. The edit is ambiguous.
	msg, err := editFile(tracker, EditInput{FilePath: path, OldString: "aba", NewString: "Z"})
	after, _ := os.ReadFile(path)
	if err == nil {
		t.Errorf("editFile(old=%q) on %q succeeded with %q and left %q; "+
			"the string occurs twice (offsets 0 and 2) so this must refuse without replace_all",
			"aba", "ababa", msg, after)
	}
}

// Read caps line COUNT, never line LENGTH. A minified bundle is one line.
func TestAdversarial_Read_SingleHugeLineIsReturnedWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bundle.min.js")
	line := strings.Repeat("x", 4<<20) // 4 MiB on one line
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readFile(files.NewTracker(), ReadInput{FilePath: path, Limit: 1})
	if err != nil {
		t.Fatalf("readFile() error = %v", err)
	}
	if len(got) > 1<<20 {
		t.Errorf("readFile(limit=1) returned %d bytes for a 1-line file; no byte cap exists, so a "+
			"minified file lands whole in the context window", len(got))
	}
}

// Same for Grep content mode: 200 lines is the only cap.
func TestAdversarial_Grep_ContentModeHasNoByteCap(t *testing.T) {
	dir := t.TempDir()
	line := strings.Repeat("needle", 400_000) // ~2.4 MiB, one matching line
	if err := os.WriteFile(filepath.Join(dir, "a.min.js"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := grepWithGo(GrepInput{Pattern: "needle", Path: dir, OutputMode: "content"})
	if err != nil {
		t.Fatalf("grepWithGo() error = %v", err)
	}
	if len(got) > 1<<20 {
		t.Errorf("grepWithGo content mode returned %d bytes from a single matching line; "+
			"maxSearchResults caps lines only", len(got))
	}
}

// rg --glob matches paths and supports **; filepath.Match matches the
// basename only and has no ** support.
func TestAdversarial_Grep_GlobSemanticsDivergeBetweenBackends(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "src/deep/a.go")
	if err := os.WriteFile(filepath.Join(dir, "src/deep/a.go"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	in := GrepInput{Pattern: "needle", Path: dir, Glob: "**/*.go", OutputMode: "files_with_matches"}

	goOut, err := grepWithGo(in)
	if err != nil {
		t.Fatalf("grepWithGo() error = %v", err)
	}
	rg, lookErr := lookRg()
	if lookErr != nil {
		t.Skip("rg not installed; cannot compare backends")
	}
	rgOut, err := grepWithRipgrep(rg, in)
	if err != nil {
		t.Fatalf("grepWithRipgrep() error = %v", err)
	}

	goHit := strings.Contains(goOut, "a.go")
	rgHit := strings.Contains(rgOut, "a.go")
	if goHit != rgHit {
		t.Errorf("glob=%q: ripgrep found the file = %v, Go fallback found it = %v\nrg:  %q\ngo:  %q",
			in.Glob, rgHit, goHit, rgOut, goOut)
	}
}

// --- things that held ---

func TestAdversarial_Edit_ReplaceAllOverlappingCountIsHonest(t *testing.T) {
	tracker := files.NewTracker()
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("aaaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(tracker, ReadInput{FilePath: path}); err != nil {
		t.Fatal(err)
	}
	msg, err := editFile(tracker, EditInput{FilePath: path, OldString: "aa", NewString: "b", ReplaceAll: true})
	if err != nil {
		t.Fatalf("editFile() error = %v", err)
	}
	after, _ := os.ReadFile(path)
	if strings.Contains(string(after), "a") {
		t.Errorf("replace_all left %q (msg %q)", after, msg)
	}
}

func TestAdversarial_Glob_SymlinkDirectoryCycle(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "f.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(dir, "a", "loop")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	done := make(chan string, 1)
	go func() {
		out, err := globFiles(GlobInput{Pattern: "**/*.go", Path: dir})
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		done <- out
	}()
	select {
	case out := <-done:
		t.Logf("globFiles returned %d bytes:\n%s", len(out), out)
	case <-time.After(10 * time.Second):
		t.Error("globFiles() did not return within 10s on a symlinked directory cycle")
	}
}

func TestAdversarial_Read_DevZero(t *testing.T) {
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("/dev/zero unavailable")
	}
	done := make(chan string, 1)
	go func() {
		out, err := readFile(files.NewTracker(), ReadInput{FilePath: "/dev/zero", Limit: 1})
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		done <- "returned " + strconv.Itoa(len(out)) + " bytes"
	}()
	select {
	case r := <-done:
		t.Logf("readFile(/dev/zero) -> %s", r)
	case <-time.After(10 * time.Second):
		t.Error("readFile(/dev/zero) did not return within 10s")
	}
}

func TestAdversarial_Tracker_ConcurrentUse(t *testing.T) {
	tracker := files.NewTracker()
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		p := filepath.Join(dir, "f"+strconv.Itoa(i%4)+".txt")
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		wg.Add(3)
		go func() { defer wg.Done(); readFile(tracker, ReadInput{FilePath: p}) }() //nolint:errcheck
		go func() { defer wg.Done(); tracker.CheckWritable(p) }()                  //nolint:errcheck
		go func() { defer wg.Done(); tracker.ReadAt(p) }()
	}
	wg.Wait()
}

func TestAdversarial_Grep_HyphenFlagsBindAtRuntime(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nNEEDLE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := `{"pattern":"needle","path":` + strconv.Quote(dir) + `,"output_mode":"content","-i":true,"-n":true}`

	got, err := newGrepTool().InvokableRun(context.Background(), args)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if !strings.Contains(got, "NEEDLE") {
		t.Errorf(`-i did not bind: output = %q`, got)
	}
	if !strings.Contains(got, ":2:") {
		t.Errorf(`-n did not bind: output = %q (want path:2:text)`, got)
	}
}
