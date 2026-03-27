package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	topologyPath string
	configDir    string
)

// NewRootCommand creates the root mesh-ctl command with global flags.
func NewRootCommand(version string) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "mesh-ctl",
		Short: "Control plane CLI for awg-mesh",
		Long:  "mesh-ctl manages AWG mesh topology, node onboarding, rotation, and monitoring.",
		SilenceUsage: true,
	}

	rootCmd.PersistentFlags().StringVarP(&topologyPath, "topology", "t", "mesh-topology.yml", "Path to topology YAML file")
	rootCmd.PersistentFlags().StringVar(&configDir, "config-dir", defaultConfigDir(), "mesh-ctl config directory")

	rootCmd.AddCommand(newVersionCommand(version))
	rootCmd.AddCommand(newEndpointCommand())
	rootCmd.AddCommand(newMasterCommand())
	rootCmd.AddCommand(newClientCommand())
	rootCmd.AddCommand(newStatusCommand())
	rootCmd.AddCommand(newTokenCommand())
	rootCmd.AddCommand(newCaptureCommand())
	rootCmd.AddCommand(newRotateCommand())
	rootCmd.AddCommand(newIPCommand())

	return rootCmd
}

func newVersionCommand(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print mesh-ctl version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("mesh-ctl version %s\n", version)
		},
	}
}

func defaultConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".mesh-ctl"
	}
	return home + "/.mesh-ctl"
}
