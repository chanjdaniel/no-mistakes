package buildinfo

import "runtime/debug"

// Set via ldflags at build time:
//
//	-ldflags "-X github.com/kunchenguid/no-mistakes/internal/buildinfo.Version=v1.0.0
//	          -X github.com/kunchenguid/no-mistakes/internal/buildinfo.Commit=abc1234
//	          -X github.com/kunchenguid/no-mistakes/internal/buildinfo.Date=2024-01-01
//	          -X github.com/kunchenguid/no-mistakes/internal/buildinfo.TelemetryHost=https://a.example.com
//	          -X github.com/kunchenguid/no-mistakes/internal/buildinfo.TelemetryWebsiteID=abc123"
// ForkBuild is "true" when this binary is a fork build rather than an
// upstream release. It defaults to "true" in this source tree so that a
// plain `go build` is protected; set to "false" via ldflags for an
// upstream-parity build. When "true", the updater suppresses the "new
// version available" nag and refuses to self-update, because the
// upstream release would overwrite fork patches.
//
// A rebuild from this source tree without the explicit override will
// carry the safe default; the trap cannot silently re-arm.
var ForkBuild = "true"

var (
	Version            = "dev"
	Commit             = "unknown"
	Date               = "unknown"
	TelemetryHost      = ""
	TelemetryWebsiteID = ""
)

func CurrentVersion() string {
	if Version != "" && Version != "dev" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
	}
	return "dev"
}

func String() string {
	return CurrentVersion() + " (" + Commit + ") " + Date
}
