package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newEndpointCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "endpoint",
		Short: "Manage endpoint nodes",
	}

	cmd.AddCommand(newEndpointPrepareCommand())
	cmd.AddCommand(newEndpointInitCommand())
	cmd.AddCommand(newEndpointRemoveCommand())

	return cmd
}

func newEndpointPrepareCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "prepare [name]",
		Short: "Generate docker-compose and token for an endpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("endpoint prepare: %s (not yet implemented)\n", args[0])
			return nil
		},
	}
}

func newEndpointInitCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "init [name]",
		Short: "Initialize endpoint via gRPC — exchange certs, configure node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("endpoint init: %s (not yet implemented)\n", args[0])
			return nil
		},
	}
}

func newEndpointRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove [name]",
		Short: "Remove an endpoint from the mesh",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("endpoint remove: %s (not yet implemented)\n", args[0])
			return nil
		},
	}
}
