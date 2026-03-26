package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newClientCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Manage client nodes",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "prepare [name]",
		Short: "Generate config for a client (linux or mikrotik)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("client prepare: %s (not yet implemented)\n", args[0])
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "init [name]",
		Short: "Initialize client via gRPC",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("client init: %s (not yet implemented)\n", args[0])
			return nil
		},
	})

	return cmd
}
