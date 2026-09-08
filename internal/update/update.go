// Package update keeps a running onegw current: it can check GitHub for a
// newer release, download the matching binary, and (outside containers)
// hand the port over to the new build with zero dropped requests. The
// handoff reuses the two invariants the server already guarantees — the
// listener sets SO_REUSEPORT, and SIGTERM drains gracefully — so a
// self-update is exactly the deploy.sh rolling restart, driven by the
// binary itself. Inside a container the filesystem is owned by the image,
// so the package only checks and prints the host-side pull/recreate
// commands.
package update

import (
	"runtime/debug"
	"strconv"
	"strings"
)

// version is stamped by the release pipeline:
//
//	go build -ldflags "-X onegw/internal/update.version=v0.6.0"
//
// Unstamped builds fall back to the Go module version (go install), then
// to "(devel)" — which semver-wise ranks below any real release, so dev
// builds always see updates.
var version string

// Version returns the running binary's version string.
func Version() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "(devel)"
}

// Semver is a parsed semantic version; prerelease orderings per semver.org
// (a prerelease sorts before its release).
type Semver struct {
	Major, Minor, Patch int
	Pre                 string // prerelease, e.g. "rc.1"; empty = release
}

// ParseSemver parses "vMAJOR.MINOR.PATCH[-PRE]"; it accepts a missing "v".
// Non-semver strings ("(devel)", git SHAs) return ok=false.
func ParseSemver(s string) (Semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	rest, pre, _ := strings.Cut(s, "-")
	if pre != "" && !validPre(pre) {
		return Semver{}, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return Semver{}, false
	}
	var v Semver
	nums := [3]int{}
	for i, p := range parts {
		if p == "" || len(p) > 9 {
			return Semver{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Semver{}, false
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	v.Pre = pre
	return v, true
}

func validPre(pre string) bool {
	for _, part := range strings.Split(pre, ".") {
		if part == "" {
			return false
		}
		for _, r := range part {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-') {
				return false
			}
		}
	}
	return true
}

// Compare returns -1, 0, or 1. Unparseable versions rank below every
// release so a dev build is always "outdated".
func Compare(a, b string) int {
	av, aok := ParseSemver(a)
	bv, bok := ParseSemver(b)
	switch {
	case aok && !bok:
		return 1
	case !aok && bok:
		return -1
	case !aok && !bok:
		return strings.Compare(a, b)
	}
	if c := cmp3(av.Major, bv.Major); c != 0 {
		return c
	}
	if c := cmp3(av.Minor, bv.Minor); c != 0 {
		return c
	}
	if c := cmp3(av.Patch, bv.Patch); c != 0 {
		return c
	}
	// Release > prerelease; otherwise dot-separated numeric/lexicographic
	// identifiers, lowest first.
	switch {
	case av.Pre == "" && bv.Pre != "":
		return 1
	case av.Pre != "" && bv.Pre == "":
		return -1
	}
	return comparePre(av.Pre, bv.Pre)
}

func comparePre(a, b string) int {
	if a == b {
		return 0
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, y := as[i], bs[i]
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil:
			if c := cmp3(xn, yn); c != 0 {
				return c
			}
		case xerr == nil:
			return -1 // numeric identifiers sort below alphanumeric
		case yerr == nil:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return cmp3(len(as), len(bs))
}

func cmp3[T int | string](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Settings is the live [update] configuration; the background service
// re-reads it on every wake so a SIGHUP reload takes effect without a
// restart. Listen and Password carry the handoff context (probe target
// and credential) so an auto-apply can verify the new process before
// draining the old one.
type Settings struct {
	Interval int64 // seconds between checks; 0 = disabled
	Auto     bool  // apply (handoff restart) when a newer release is found
	Repo     string
	Listen   string // gateway listen address; probe target for the handoff
	Password string // admin password used to verify the new process
}
