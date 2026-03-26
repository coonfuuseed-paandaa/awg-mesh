package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	rootCmd := newRootCommand()
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "mesh-ctl command failed: %v\n", err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "mesh-ctl",
		Short: "Control plane CLI for awg-mesh",
	}
	rootCmd.AddCommand(newVersionCommand())
	return rootCmd
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print mesh-ctl version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("mesh-ctl version %s\n", version)
		},
	}
}
