package versioninfo

import (
	"runtime/debug"
	"strings"
)

// Version and Commit identify the running CLI build.
//
// Release and `mise build` binaries stamp these via ldflags
// (-X ...versioninfo.Version=...). For binaries built without those ldflags --
// notably `go install github.com/entireio/cli/cmd/entire@<version>` and plain
// `go build`/`go install ./...` -- they are recovered from Go's embedded build
// info instead, so the CLI still self-reports a real version and commit.
var (
	Version = "dev"
	Commit  = "unknown"
)

func init() {
	info, ok := debug.ReadBuildInfo()
	Version, Commit = resolve(Version, Commit, info, ok)
}

// resolve fills Version/Commit from build info only when ldflags left them at
// their defaults; an explicit ldflags stamp always wins. A module install
// (`@<version>`) carries the version as info.Main.Version, while a local build
// reports "(devel)" there but records the commit under the vcs.* settings.
func resolve(version, commit string, info *debug.BuildInfo, ok bool) (string, string) {
	if version != "dev" || !ok || info == nil {
		return version, commit
	}

	if v := info.Main.Version; v != "" && v != "(devel)" {
		version = strings.TrimPrefix(v, "v") // match GoReleaser's {{.Version}}
	}

	var revision string
	var dirty bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if revision != "" {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		if dirty {
			revision += "-dirty"
		}
		commit = revision
	}

	return version, commit
}
