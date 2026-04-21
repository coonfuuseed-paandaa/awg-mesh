package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

var (
	topologyPath string
	configDir    string
)

// NewRootCommand creates the root mesh-ctl command with global flags.
func NewRootCommand(version string) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:          "mesh-ctl",
		Short:        "Control plane CLI for awg-mesh",
		Long:         "mesh-ctl manages AWG mesh topology, node onboarding, rotation, and monitoring.",
		SilenceUsage: true,
		// B2 fix: resolve --topology against --config-dir when the path is bare
		// (no directory separator). This lets users run `mesh-ctl -t mesh-topology.yml`
		// from any working directory while keeping all config co-located under
		// --config-dir instead of whichever CWD the shell happens to be in.
		// Resolution order:
		//   1. Absolute path → used as-is.
		//   2. Relative path with at least one separator (./foo, ../foo) → used as-is.
		//   3. Bare filename (e.g. "mesh-topology.yml") → resolved against configDir.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if !filepath.IsAbs(topologyPath) && filepath.Dir(topologyPath) == "." {
				topologyPath = filepath.Join(configDir, topologyPath)
			}
			return nil
		},
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
	rootCmd.AddCommand(newConfigCommand())
	rootCmd.AddCommand(newRoutingCommand())
	rootCmd.AddCommand(newBootstrapCommand())
	rootCmd.AddCommand(newInspectCommand())
	rootCmd.AddCommand(newReconcileCommand())

	upgradeCmd := newUpgradeCommand()
	upgradeCmd.AddCommand(newUpgradeComposeCommand())
	rootCmd.AddCommand(upgradeCmd)

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
	return filepath.Join(home, ".mesh-ctl")
}
