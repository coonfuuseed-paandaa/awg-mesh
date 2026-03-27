package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/thebtf/awg-mesh/cmd/mesh-ctl/cmd"
)

func main() {
	rootCmd := cmd.NewRootCommand(version())
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "mesh-ctl: %v\n", err)
		os.Exit(1)
	}
}

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}

	// go install ...@v0.2.0 → "v0.2.0"
	if v := info.Main.Version; v != "" && v != "(devel)" && !strings.Contains(v, "-0.") {
		return v
	}

	// go install ./cmd/mesh-ctl from git repo → git hash
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
		v := revision
		if len(v) > 8 {
			v = v[:8]
		}
		if modified == "true" {
			v += "-dirty"
		}
		return v
	}

	return "dev"
}
