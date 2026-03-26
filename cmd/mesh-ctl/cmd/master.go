package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newMasterCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "master",
		Short: "Manage master nodes",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "prepare [name]",
		Short: "Generate docker-compose and token for a master",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("master prepare: %s (not yet implemented)\n", args[0])
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "init [name]",
		Short: "Initialize master via gRPC",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("master init: %s (not yet implemented)\n", args[0])
			return nil
		},
	})

	return cmd
}
