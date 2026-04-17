package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	grpcclient "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
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
	var showToken bool

	cmd := &cobra.Command{
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

			// Use CA-verified TLS for token rotation (post-Init, CA cert is available).
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
			tokenPath := filepath.Join(nd, "token")

			if showToken {
				fmt.Fprintln(os.Stdout, newToken) // OK: gated behind --show-token flag
				logger := zerolog.New(os.Stderr).With().Timestamp().Logger()
				logger.Warn().
					Str("event", "show_token_flag").
					Str("command", "token rotate").
					Msg("token emitted to stdout; prefer 'cat <token-path>' for scripted retrieval")
			} else {
				fmt.Fprintf(os.Stderr, "Token rotated for %q. Saved to %s.\n", name, tokenPath)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&showToken, "show-token", false, "print raw token to stdout (default: save to disk only)")
	return cmd
}

func loadNodeHost(nodeDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(nodeDir, "host"))
	if err != nil {
		return "", fmt.Errorf("read host file: %w", err)
	}
	return string(data), nil
}
