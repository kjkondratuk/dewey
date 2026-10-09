package release

// seed is the first version a repository with no tags releases. There is no
// base to bump from, so this is chosen rather than computed, and the size of
// the bump the commits call for does not change it.
var seed = Version{Minor: 1}

// LatestStable returns the highest stable version in versions, or the zero
// Version when there is none. Prereleases are skipped so that publishing
// v1.3.0-alpha.1 does not move the base the next prerelease counts from.
func LatestStable(versions []Version) Version {
	var latest Version
	for _, v := range versions {
		if v.Channel != Stable {
			continue
		}
		if latest.IsZero() || latest.Less(v) {
			latest = v
		}
	}
	return latest
}

// Next returns the version to release and whether there is anything to
// release at all.
//
// latest is the newest stable version, or the zero Version for a repository
// with no tags. commits are those since latest. ch selects the channel.
// existing is every version tag in the repository, used only to continue a
// prerelease counter.
func Next(latest Version, commits []Commit, ch Channel, existing []Version) (Version, bool) {
	bump := HighestBump(commits)

	if ch == Stable {
		// A merge carrying only docs, chore, test, ci, build, style or
		// refactor commits ships nothing, so it must not mint a version whose
		// binary is identical to the last one. An empty range lands here too,
		// which is what makes re-running the release workflow on an already
		// tagged commit safe.
		if bump == BumpNone {
			return Version{}, false
		}
		if latest.IsZero() {
			return seed, true
		}
		return apply(latest, bump), true
	}

	// A manual prerelease is an explicit intent to ship something for
	// testing. Refusing it because the range holds only a refactor would
	// defeat the purpose of having the pipeline.
	if bump == BumpNone {
		bump = BumpPatch
	}
	base := seed
	if !latest.IsZero() {
		base = apply(latest, bump)
	}
	return Version{
		Major:   base.Major,
		Minor:   base.Minor,
		Patch:   base.Patch,
		Channel: ch,
		Counter: nextCounter(existing, base, ch),
	}, true
}

// apply increments v by b, zeroing the components below the one that moved.
func apply(v Version, b Bump) Version {
	switch b {
	case BumpMajor:
		return Version{Major: v.Major + 1}
	case BumpMinor:
		return Version{Major: v.Major, Minor: v.Minor + 1}
	default:
		return Version{Major: v.Major, Minor: v.Minor, Patch: v.Patch + 1}
	}
}

// nextCounter continues the prerelease sequence for one base and channel.
// Counters are scoped to the base, so v1.3.0-alpha.2 is followed by
// v1.4.0-alpha.1 once v1.3.0 has shipped.
func nextCounter(existing []Version, base Version, ch Channel) int {
	highest := 0
	for _, v := range existing {
		if v.Channel == ch && v.Base() == base && v.Counter > highest {
			highest = v.Counter
		}
	}
	return highest + 1
}
