package memory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitRemoteTimeout bounds the org-detection subprocess. Reading a remote URL
// only touches git config, so anything slower than this is a stalled
// filesystem rather than slow work.
const gitRemoteTimeout = 2 * time.Second

// stackMarkers maps a file that identifies a stack to the tag it implies.
var stackMarkers = map[string]string{
	"go.mod":           "go",
	"package.json":     "node",
	"Cargo.toml":       "rust",
	"pyproject.toml":   "python",
	"requirements.txt": "python",
}

// DetectContext infers what the work in dir is, so memories can be scoped to
// it.
//
// The org comes from the git remote and nothing else. An earlier design let a
// project's own config file declare it, which was worse in two ways: a
// checkout's contents come from whoever authored the repo, so a cloned repo
// could decide which of the user's org-scoped memories were loaded; and config
// lookup did not walk up to the repo root while `git -C` does, so running from
// a subdirectory silently swapped one source for the other. One source removes
// both problems — at the cost that a repo with no remote cannot be
// org-scoped, which fails closed.
func DetectContext(dir string) Context {
	// Markers are looked for upward from dir, because org detection already
	// walks up (git does it) and so does project config. Statting only dir
	// meant running from a subdirectory kept the org but silently lost the
	// stack tag, so every tags-filtered memory stopped applying one level
	// down from the repo root.
	var tags []string
	for _, root := range markerSearchPath(dir) {
		for marker, tag := range stackMarkers {
			if _, err := os.Stat(filepath.Join(root, marker)); err == nil && !contains(tags, tag) {
				tags = append(tags, tag)
			}
		}
	}
	sortStrings(tags)

	// A remote of github.com/Acme/repo yields "Acme"; normalizing here is what
	// lets it match a memory written as "org: acme". Both ends have to
	// normalize or the comparison simply never succeeds.
	org := normalizeTag(parseOrgFromRemote(gitRemote(dir)))
	return Context{Org: org, Tags: normalizeTags(tags)}
}

// markerSearchPath returns dir and its ancestors up to the repository root,
// nearest first. It stops at a .git directory so detection cannot wander into
// unrelated parents, and is bounded by the filesystem root otherwise.
func markerSearchPath(dir string) []string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return []string{dir}
	}

	var path []string
	for cur := abs; ; {
		path = append(path, cur)
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return path
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return path
		}
		cur = parent
	}
}

// gitRemote is bounded because DetectContext runs before the TUI exists: on a
// stalled network checkout an unbounded subprocess would hang dewey at launch
// with nothing on screen to explain it.
func gitRemote(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitRemoteTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// parseOrgFromRemote pulls the owner segment out of a git remote URL, covering
// the https, scp-style, and ssh:// forms.
func parseOrgFromRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	remote = strings.TrimSuffix(remote, ".git")

	// scp-style: git@host:owner/repo
	if at := strings.Index(remote, "@"); at >= 0 && !strings.Contains(remote, "://") {
		if colon := strings.Index(remote[at:], ":"); colon >= 0 {
			return firstSegment(remote[at+colon+1:])
		}
		return ""
	}

	// URL forms: scheme://[user@]host/owner/repo
	if i := strings.Index(remote, "://"); i >= 0 {
		rest := remote[i+len("://"):]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			return firstSegment(rest[slash+1:])
		}
	}
	return ""
}

func firstSegment(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	if slash := strings.Index(path, "/"); slash >= 0 {
		return path[:slash]
	}
	// A bare segment with no repo after it isn't an owner.
	return ""
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// sortStrings keeps map-derived tag order stable so output is deterministic.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
