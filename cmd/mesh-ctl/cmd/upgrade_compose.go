package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/upgrade"
	"github.com/spf13/cobra"
)

// newUpgradeComposeCommand returns `mesh-ctl upgrade compose <old-file>` (F6).
// It reads an arbitrary docker-compose YAML file, detects (or accepts an
// explicit override of) its schema version, migrates it to the current schema,
// and writes the result either to stdout or in-place (with a .bak backup).
func newUpgradeComposeCommand() *cobra.Command {
	var (
		fromSchema string
		inPlace    bool
	)

	cmd := &cobra.Command{
		Use:   "compose <old-file>",
		Short: "Migrate an older-format docker-compose.yml to the current schema",
		Long: `compose reads an older-format awg-mesh docker-compose file, detects
its schema version, and migrates it to the current schema:
  - MESH_TOKEN (v1.5.1) or MESH_TOKEN_HASH + command: (v1.6.0) → env-only
  - MESH_CONFIG_DIR=/config volume mount added (v1.9.0 → current)
  - Operator comments are preserved

By default the migrated file is written to stdout.
Use --in-place to rewrite the file in-place (original is saved as <file>.bak).
Use --from-schema to override auto-detection when the heuristic is ambiguous.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := strings.TrimSpace(args[0])
			if path == "" {
				return fmt.Errorf("file path must not be empty")
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}

			var schema upgrade.SchemaVersion
			if fromSchema != "" {
				schema, err = upgrade.ParseSchemaVersion(fromSchema)
				if err != nil {
					return fmt.Errorf("--from-schema: %w", err)
				}
			} else {
				schema, err = upgrade.DetectSchema(data)
				if err != nil {
					return fmt.Errorf("detect schema: %w", err)
				}
			}

			if schema == upgrade.SchemaCurrent {
				fmt.Fprintln(cmd.ErrOrStderr(), "already current schema, nothing to do")
				return nil
			}

			migrated, err := upgrade.MigrateCompose(data, schema)
			if err != nil {
				return fmt.Errorf("migrate compose: %w", err)
			}

			if !inPlace {
				_, err = cmd.OutOrStdout().Write(migrated)
				return err
			}

			// In-place mode: move original to <path>.bak, write result to original.
			bakPath := path + ".bak"
			if err := os.Rename(path, bakPath); err != nil {
				return fmt.Errorf("backup %s → %s: %w", path, bakPath, err)
			}
			if err := os.WriteFile(path, migrated, 0600); err != nil {
				// Attempt to restore backup before surfacing the error.
				_ = os.Rename(bakPath, path)
				return fmt.Errorf("write migrated compose to %s: %w", path, err)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "migrated %s (schema %s → current); original backed up to %s\n",
				path, schema, bakPath)
			return nil
		},
	}

	cmd.Flags().StringVar(&fromSchema, "from-schema", "", "Override schema version (v1.5.1|v1.6.0|v1.9.0|current)")
	cmd.Flags().BoolVar(&inPlace, "in-place", false, "Rewrite file in-place; original saved as <file>.bak")

	return cmd
}
