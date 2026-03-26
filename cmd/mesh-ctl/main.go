package main

import (
	"fmt"
	"os"

	"github.com/thebtf/awg-mesh/cmd/mesh-ctl/cmd"
)

var version = "dev"

func main() {
	rootCmd := cmd.NewRootCommand(version)
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "mesh-ctl: %v\n", err)
		os.Exit(1)
	}
}
