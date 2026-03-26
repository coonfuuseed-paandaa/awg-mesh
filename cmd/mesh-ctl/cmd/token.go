package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newTokenCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage MESH_TOKEN rotation",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "rotate [node]",
		Short: "Rotate MESH_TOKEN for a node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("token rotate: %s (not yet implemented)\n", args[0])
			return nil
		},
	})

	return cmd
}
