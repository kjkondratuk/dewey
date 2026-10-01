package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// stackMarkers maps a file that identifies a stack to the tag it implies.
var stackMarkers = map[string]string{
	"go.mod":           "go",
	"package.json":     "node",
	"Cargo.toml":       "rust",
	"pyproject.toml":   "python",
	"requirements.txt": "python",
}

// DetectContext infers what the work in dir is, so memories can be scoped to
// it. projectOrg comes from the project's config file and wins outright: git
// remote parsing is too fragile to be the only basis for keeping one
// organization's conventions out of another's work.
func DetectContext(dir, projectOrg string) Context {
	var tags []string
	for marker, tag := range stackMarkers {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil && !contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	sortStrings(tags)

	org := normalizeTag(projectOrg)
	if org == "" {
		// A remote of github.com/Acme/repo yields "Acme"; normalizing here is
		// what lets it match a memory written as "org: acme". Both ends have to
		// normalize or the comparison simply never succeeds.
		org = normalizeTag(parseOrgFromRemote(gitRemote(dir)))
	}
	return Context{Org: org, Tags: normalizeTags(tags)}
}

func gitRemote(dir string) string {
	cmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
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
