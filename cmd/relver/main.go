// Command relver computes the version to release and reports it to GitHub
// Actions. The decisions live in internal/release; this binary only talks to
// git and to $GITHUB_OUTPUT, so it can be run by hand to see what a release
// would do.
//
//	go run ./cmd/relver                  # stable
//	go run ./cmd/relver --channel=alpha  # prerelease
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/kjkondratuk/dewey/internal/release"
)

func main() {
	channel := flag.String("channel", "", "prerelease channel: alpha or beta; empty for a stable release")
	flag.Parse()

	ch := release.Channel(*channel)
	switch ch {
	case release.Stable, release.Alpha, release.Beta:
	default:
		fatal(fmt.Errorf("unknown channel %q: want alpha, beta, or an empty value", *channel))
	}

	existing, err := existingVersions()
	if err != nil {
		fatal(err)
	}
	latest := release.LatestStable(existing)

	commits, err := commitsSince(latest)
	if err != nil {
		fatal(err)
	}

	version, ok := release.Next(latest, commits, ch, existing)
	if err := report(version, ok); err != nil {
		fatal(err)
	}
}

// existingVersions reads every tag in the repository that is a version,
// ignoring any that are not.
func existingVersions() ([]release.Version, error) {
	out, err := git("tag", "--list")
	if err != nil {
		return nil, err
	}
	var versions []release.Version
	for _, tag := range strings.Fields(out) {
		if v, ok := release.ParseVersion(tag); ok {
			versions = append(versions, v)
		}
	}
	return versions, nil
}

// commitsSince reads the commits after latest, or the whole history when the
// repository has no stable tag yet.
func commitsSince(latest release.Version) ([]release.Commit, error) {
	args := []string{"log", "--format=%s%x00%b%x1e"}
	if !latest.IsZero() {
		args = append(args, latest.String()+"..HEAD")
	}
	out, err := git(args...)
	if err != nil {
		return nil, err
	}
	return release.ParseLog(out), nil
}

// report writes the decision to stdout for the workflow log, and to
// $GITHUB_OUTPUT for later steps. Outside Actions that variable is unset and
// only stdout is written, which is what makes this runnable by hand.
func report(v release.Version, ok bool) error {
	lines := fmt.Sprintf("release=%t\n", ok)
	if ok {
		lines += fmt.Sprintf("version=%s\n", v)
	}
	fmt.Print(lines)

	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(lines); err != nil {
		return err
	}
	return f.Close()
}

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		// .Output() captures stderr into ExitError.Stderr, but ExitError's
		// own message is only "exit status N". Without this, a CI failure
		// logs the status and discards the one line explaining it.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %w: %s",
				strings.Join(args, " "), err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "relver:", err)
	os.Exit(1)
}
