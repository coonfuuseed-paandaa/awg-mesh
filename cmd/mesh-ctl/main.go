package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/thebtf/awg-mesh/cmd/mesh-ctl/cmd"
)

// version is set via ldflags at build time: -X main.version=v0.1.0
// Falls back to module version from go install, then "dev".
var version = "dev"

func main() {
	rootCmd := cmd.NewRootCommand(resolveVersion())
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "mesh-ctl: %v\n", err)
		os.Exit(1)
	}
}

func resolveVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}
