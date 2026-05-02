package main

import (
	"context"
	"os"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/clientd"
)

func main() {
	os.Exit(clientd.RunCommand(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
