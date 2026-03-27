package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	grpcclient "github.com/thebtf/awg-mesh/pkg/grpc"
	pkgtls "github.com/thebtf/awg-mesh/pkg/tls"
	proto "github.com/thebtf/awg-mesh/proto"
)

func newTokenCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage MESH_TOKEN rotation",
	}

	cmd.AddCommand(newTokenRotateCommand())

	return cmd
}

func newTokenRotateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rotate [node]",
		Short: "Rotate MESH_TOKEN for a node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			nd := nodeDir(configDir, name)
			oldToken, err := loadToken(nd)
			if err != nil {
				return fmt.Errorf("load current token for %q: %w", name, err)
			}

			host, err := loadNodeHost(nd)
			if err != nil {
				return fmt.Errorf("load node host: %w", err)
			}

			newToken, err := pkgtls.GenerateToken()
			if err != nil {
				return fmt.Errorf("generate new token: %w", err)
			}

			newHash, err := pkgtls.HashToken(newToken)
			if err != nil {
				return fmt.Errorf("hash new token: %w", err)
			}

			client, err := grpcclient.NewClient(grpcclient.ClientConfig{
				Target:     host + ":9090",
				CACertPath: caPath(configDir),
				Token:      oldToken,
			})
			if err != nil {
				return fmt.Errorf("create gRPC client: %w", err)
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client: %v\n", closeErr)
				}
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			resp, err := client.Agent().RotateToken(ctx, &proto.RotateTokenRequest{
				NewTokenHash: newHash,
			})
			if err != nil {
				return fmt.Errorf("rotate token RPC: %w", err)
			}
			if !resp.Success {
				return fmt.Errorf("token rotation failed on node")
			}

			if err := pkgtls.SaveTokenHash(nd, newHash); err != nil {
				return fmt.Errorf("save new token hash locally: %w", err)
			}
			if err := saveToken(nd, newToken); err != nil {
				return fmt.Errorf("save new token locally: %w", err)
			}

			fmt.Printf("Token rotated for %q.\nNew token: %s\n", name, newToken)
			return nil
		},
	}
}

func loadNodeHost(nodeDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(nodeDir, "host"))
	if err != nil {
		return "", fmt.Errorf("read host file: %w", err)
	}
	return string(data), nil
}
