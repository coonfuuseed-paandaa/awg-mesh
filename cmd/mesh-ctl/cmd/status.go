package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Query status of all mesh nodes via gRPC",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("status: not yet implemented")
			return nil
		},
	}
}
