package main

import (
	"flag"
	"fmt"
	"os"
)

var version = "dev"

const (
	modeMaster   = "master"
	modeEndpoint = "endpoint"
	modeClient   = "client"
)

func main() {
	mode := flag.String("mode", modeMaster, "Node mode: master|endpoint|client")
	flag.Parse()

	if !isValidMode(*mode) {
		fmt.Fprintf(os.Stderr, "invalid --mode %q; valid values: %s, %s, %s\n", *mode, modeMaster, modeEndpoint, modeClient)
		os.Exit(1)
	}

	fmt.Printf("awg-mesh-node version %s mode %s\n", version, *mode)
}

func isValidMode(mode string) bool {
	switch mode {
	case modeMaster, modeEndpoint, modeClient:
		return true
	default:
		return false
	}
}
