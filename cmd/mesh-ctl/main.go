package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/cmd/mesh-ctl/cmd"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/awgmesh"
)

func main() {
	rootCmd := cmd.NewRootCommand(version())
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "mesh-ctl: %v\n", err)
		os.Exit(1)
	}
}

// versionFromBuild is injected at build time via
//
//	go build -ldflags "-X main.versionFromBuild=<ref>"
//
// Empty when not injected — falls through to debug.ReadBuildInfo() cascade
// which handles the "go install module@version" path (clean release tag) and
// the local clone path (base tag + short SHA + optional "dirty" marker).
var versionFromBuild = ""

func version() string {
	if versionFromBuild != "" {
		return versionFromBuild
	}
	info, ok := debug.ReadBuildInfo()
	return versionFromBuildInfo(info, ok)
}

func versionFromBuildInfo(info *debug.BuildInfo, ok bool) string {
	if !ok {
		return "dev"
	}

	// Extract VCS info for local builds
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}

	if revision != "" {
		short := revision
		if len(short) > 8 {
			short = short[:8]
		}
		if modified == "true" {
			return awgmesh.Version + " (" + short + ", dirty)"
		}
		return awgmesh.Version + " (" + short + ")"
	}

	// go install ...@v0.2.0 → "v0.2.0" (clean release tag)
	modVer := info.Main.Version
	if modVer != "" && modVer != "(devel)" && !strings.Contains(modVer, "-0.") {
		return modVer
	}

	if modVer != "" && modVer != "(devel)" && strings.Contains(modVer, "-0.") {
		// "v0.2.1-0.20260327..." → "v0.2.1"
		if idx := strings.Index(modVer, "-0."); idx > 0 {
			return modVer[:idx]
		}
	}

	return awgmesh.Version
}
