// Package buildinfo carries the values stamped into the binary at link
// time. GoReleaser sets these via -ldflags -X; a plain `go build` or
// `go run` leaves them empty, in which case we fall back to the module
// metadata the toolchain embeds automatically. That fallback matters:
// a developer running `go run ./cmd/xcloud version` should see
// something truthful rather than a bare "dev".
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Stamped by the linker. Do not set these anywhere else.
var (
	Version = ""
	Commit  = ""
	Date    = ""
)

// Resolve returns the version, commit and build date, filling in from
// the embedded module metadata whatever the linker did not stamp.
func Resolve() (version, commit, date string) {
	version, commit, date = Version, Commit, Date

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return fallback(version), commit, date
	}

	// A `go install module@version` build records the real version here;
	// a local `go build` records "(devel)", which is no more useful than
	// our own placeholder, so it is filtered out.
	if version == "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if commit == "" {
				commit = s.Value
			}
		case "vcs.time":
			if date == "" {
				date = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				commit += "-dirty"
			}
		}
	}
	return fallback(version), commit, date
}

func fallback(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

// UserAgent is sent on every API request. The server logs it, so keep it
// parseable and stable: changing the shape breaks whatever dashboards
// slice adoption by client version.
func UserAgent() string {
	version, _, _ := Resolve()
	return fmt.Sprintf("xcloud-cli/%s (%s/%s; %s)",
		version, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// String renders the human-facing one-liner for `xcloud version`.
func String() string {
	version, commit, date := Resolve()
	s := "xcloud " + version
	if commit != "" {
		s += " (commit " + shortCommit(commit)
		if date != "" {
			s += ", built " + date
		}
		s += ")"
	}
	return s + fmt.Sprintf(" %s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// shortCommit abbreviates a full SHA to 7 chars, preserving any
// "-dirty" suffix Resolve may have appended.
func shortCommit(commit string) string {
	dirty := strings.HasSuffix(commit, "-dirty")
	sha := strings.TrimSuffix(commit, "-dirty")
	if len(sha) > 7 {
		sha = sha[:7]
	}
	if dirty {
		return sha + "-dirty"
	}
	return sha
}
