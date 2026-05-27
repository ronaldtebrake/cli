package versioninfo

import (
	"runtime/debug"
	"testing"
)

func buildInfo(mainVersion string, settings ...debug.BuildSetting) *debug.BuildInfo {
	return &debug.BuildInfo{
		Main:     debug.Module{Path: "github.com/entireio/cli", Version: mainVersion},
		Settings: settings,
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name        string
		version     string
		commit      string
		info        *debug.BuildInfo
		ok          bool
		wantVersion string
		wantCommit  string
	}{
		{
			name:        "ldflags stamp wins over build info",
			version:     "0.6.2",
			commit:      "abc1234",
			info:        buildInfo("v9.9.9"),
			ok:          true,
			wantVersion: "0.6.2",
			wantCommit:  "abc1234",
		},
		{
			name:        "module install recovers tagged version",
			version:     "dev",
			commit:      "unknown",
			info:        buildInfo("v0.6.2-nightly.202605160654.ddf1a331"),
			ok:          true,
			wantVersion: "0.6.2-nightly.202605160654.ddf1a331",
			wantCommit:  "unknown",
		},
		{
			name:    "local build recovers commit from vcs settings",
			version: "dev",
			commit:  "unknown",
			info: buildInfo("(devel)",
				debug.BuildSetting{Key: "vcs.revision", Value: "ddf1a331c0ffee1234567890abcdef0987654321"},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"},
			),
			ok:          true,
			wantVersion: "dev",
			wantCommit:  "ddf1a331c0ff-dirty",
		},
		{
			name:    "clean local build omits dirty suffix",
			version: "dev",
			commit:  "unknown",
			info: buildInfo("(devel)",
				debug.BuildSetting{Key: "vcs.revision", Value: "ddf1a331c0ffee1234567890abcdef0987654321"},
				debug.BuildSetting{Key: "vcs.modified", Value: "false"},
			),
			ok:          true,
			wantVersion: "dev",
			wantCommit:  "ddf1a331c0ff",
		},
		{
			name:        "missing build info leaves defaults",
			version:     "dev",
			commit:      "unknown",
			info:        nil,
			ok:          false,
			wantVersion: "dev",
			wantCommit:  "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotVersion, gotCommit := resolve(tt.version, tt.commit, tt.info, tt.ok)
			if gotVersion != tt.wantVersion {
				t.Errorf("version = %q, want %q", gotVersion, tt.wantVersion)
			}
			if gotCommit != tt.wantCommit {
				t.Errorf("commit = %q, want %q", gotCommit, tt.wantCommit)
			}
		})
	}
}
