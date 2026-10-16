// Package release computes the version to release from conventional commits.
// Every decision lives in a pure function, so release logic is verified by
// tests rather than by pushing tags and seeing what happens -- a wrong bump
// leaves a published tag behind.
package release

import (
	"fmt"
	"regexp"
	"strconv"
)

// Channel distinguishes a stable release from a prerelease line.
type Channel string

const (
	Stable Channel = ""
	Alpha  Channel = "alpha"
	Beta   Channel = "beta"
)

// Version is a semver version, optionally on a prerelease channel. Counter is
// the per-base prerelease number and is zero for a stable version.
type Version struct {
	Major, Minor, Patch int
	Channel             Channel
	Counter             int
}

// versionPattern deliberately admits only the two prerelease channels this
// project publishes. A tag like v1.2.3-rc.1 is not a version dewey produced,
// so it must not be mistaken for one when continuing a counter.
var versionPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-(alpha|beta)\.(\d+))?$`)

// ParseVersion reads a tag like v1.2.3 or v1.3.0-alpha.2. The bool reports
// whether the tag was a version at all, since a repository may carry tags
// that are not.
func ParseVersion(tag string) (Version, bool) {
	m := versionPattern.FindStringSubmatch(tag)
	if m == nil {
		return Version{}, false
	}
	v := Version{Major: atoi(m[1]), Minor: atoi(m[2]), Patch: atoi(m[3])}
	if m[4] != "" {
		v.Channel = Channel(m[4])
		v.Counter = atoi(m[5])
	}
	return v, true
}

// atoi is safe without an error check because versionPattern has already
// proven every captured group is a run of digits.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func (v Version) String() string {
	if v.Channel == Stable {
		return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	}
	return fmt.Sprintf("v%d.%d.%d-%s.%d", v.Major, v.Minor, v.Patch, v.Channel, v.Counter)
}

// Base strips the prerelease channel, leaving the version it leads up to.
func (v Version) Base() Version {
	return Version{Major: v.Major, Minor: v.Minor, Patch: v.Patch}
}

// IsZero reports the absence of a version, which is how "no tags yet"
// travels through this package. v0.0.0 is never produced, so the zero value
// is unambiguous.
func (v Version) IsZero() bool {
	return v == Version{}
}

// Less orders versions by semver precedence, with a prerelease sorting below
// the release it leads to.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	if v.Patch != o.Patch {
		return v.Patch < o.Patch
	}
	if (v.Channel == Stable) != (o.Channel == Stable) {
		return o.Channel == Stable
	}
	if v.Channel != o.Channel {
		return v.Channel < o.Channel
	}
	return v.Counter < o.Counter
}
