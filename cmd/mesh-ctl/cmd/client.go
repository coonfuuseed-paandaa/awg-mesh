package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	grpcclient "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/mikrotik"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/node"
	pkgtls "github.com/coonfuuseed-paandaa/awg-mesh/pkg/tls"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

func newClientCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Manage client nodes",
	}

	cmd.AddCommand(newClientPrepareCommand())
	cmd.AddCommand(newClientInitCommand())
	cmd.AddCommand(newClientRemoveCommand())

	return cmd
}

func saveClientInitState(configDir string, state node.ClientState) error {
	trimmedDir := strings.TrimSpace(configDir)
	if trimmedDir == "" {
		return fmt.Errorf("config directory is required")
	}

	data, err := yaml.Marshal(&state)
	if err != nil {
		return fmt.Errorf("marshal client init state: %w", err)
	}

	path := filepath.Join(trimmedDir, "client-state.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create client init state directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write client init state: %w", err)
	}
	return nil
}

func newClientPrepareCommand() *cobra.Command {
	var useTraefik bool
	var showToken bool
	var imageFlag string

	cmd := &cobra.Command{
		Use:   "prepare [name]",
		Short: "Generate config for a client (linux or mikrotik)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			// Validate --image early so the user gets a clear error before any
			// expensive operations (topology load, CA, token generation).
			if imageFlag != "" {
				if err := validateImageRef(imageFlag); err != nil {
					return fmt.Errorf("invalid --image: %w", err)
				}
			}

			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			client := topo.FindClient(name)
			if client == nil {
				return fmt.Errorf("client %q not found in topology", name)
			}

			switch client.Type {
			case "linux":
				_, _, err := ensureCA(configDir)
				if err != nil {
					return fmt.Errorf("ensure CA: %w", err)
				}

				token, err := pkgtls.GenerateToken()
				if err != nil {
					return fmt.Errorf("generate token: %w", err)
				}

				hash, err := pkgtls.HashToken(token)
				if err != nil {
					return fmt.Errorf("hash token: %w", err)
				}

				nd := nodeDir(configDir, name)
				if err := pkgtls.SaveTokenHash(nd, hash); err != nil {
					return fmt.Errorf("save token hash: %w", err)
				}

				if err := saveToken(nd, token); err != nil {
					return fmt.Errorf("save token: %w", err)
				}

				data := struct {
					Name      string
					OverlayIP string
					Image     string
					TokenHash string
				}{
					Name:      client.Name,
					OverlayIP: client.OverlayIP,
					Image:     resolveImage(imageFlag, topo.Defaults.Image.Client, "ghcr.io/coonfuuseed-paandaa/awg-mesh-client:latest", "defaults.image.client"),
					TokenHash: hash,
				}

				// B3 fix: write output to configDir/clients/<name>/ instead of CWD.
				// Writing to CWD caused files to scatter across wherever the operator
				// happened to run mesh-ctl; keeping them in configDir makes them
				// co-located with token and pubkey files for the same client.
				clientOutDir := filepath.Join(configDir, "clients", name)
				if err := os.MkdirAll(clientOutDir, 0700); err != nil {
					return fmt.Errorf("create client output dir %q: %w", clientOutDir, err)
				}
				outputPath := filepath.Join(clientOutDir, name+"-docker-compose.yml")
				templateName := "docker-compose.client.yml.tmpl"
				if useTraefik {
					templateName = "docker-compose.client.traefik.yml.tmpl"
				}
				clientTemplate, err := loadTemplate(templateName)
				if err != nil {
					return fmt.Errorf("load client compose template: %w", err)
				}
				if err := renderDockerCompose(clientTemplate, data, outputPath); err != nil {
					return fmt.Errorf("render docker-compose: %w", err)
				}

				tokenPath := filepath.Join(nd, "token")
				printNextSteps("client", name, token, tokenPath, outputPath, useTraefik, showToken)

			case "mikrotik":
				nd := nodeDir(configDir, name)
				if err := os.MkdirAll(nd, 0755); err != nil {
					return fmt.Errorf("create node directory: %w", err)
				}

				token, err := pkgtls.GenerateToken()
				if err != nil {
					return fmt.Errorf("generate token: %w", err)
				}

				hash, err := pkgtls.HashToken(token)
				if err != nil {
					return fmt.Errorf("hash token: %w", err)
				}

				if err := pkgtls.SaveTokenHash(nd, hash); err != nil {
					return fmt.Errorf("save token hash: %w", err)
				}

				if err := saveToken(nd, token); err != nil {
					return fmt.Errorf("save token: %w", err)
				}

				// Derive CAPS naming convention for RouterOS.
				containerName := mikrotik.DeriveContainerName(name)

				// Resolve veth name and gateway from topology veth block,
				// falling back to CAPS container name and CGN subnet.
				vethName := containerName
				vethGateway := "" // empty = deriveVethAddressAndGateway defaults to 100.127.0.x
				if client.Veth != nil {
					if client.Veth.Name != "" {
						vethName = client.Veth.Name
					}
					if client.Veth.Gateway != "" {
						vethGateway = client.Veth.Gateway
					}
				}

				// DNS: topology override or safe defaults.
				dns := []string{"1.1.1.1", "8.8.8.8"}
				if client.DNS != nil && client.DNS.Upstream != "" {
					dns = strings.Split(client.DNS.Upstream, ",")
				}

				grpcPort := client.GRPCPort
				if grpcPort == 0 {
					grpcPort = 9090
				}

				ds := mikrotik.DeployScript{
					TopologyName:  name,
					ContainerName: containerName,
					Image:         resolveImage(imageFlag, topo.Defaults.Image.Client, "ghcr.io/coonfuuseed-paandaa/awg-mesh-client:latest", "defaults.image.client"),
					Veth:          vethName,
					VethGateway:   vethGateway,
					OverlayIP:     client.OverlayIP,
					OverlayNet:    topo.Overlay.Space,
					TokenHash:     hash,
					DNS:           dns,
					GRPCPort:      grpcPort,
					StorageRoot:   clientStorageRoot(client),
				}

				rsc, err := mikrotik.GenerateDeployRSC(ds)
				if err != nil {
					return fmt.Errorf("generate RouterOS script: %w", err)
				}

				// B3/B23 fix: write .rsc to configDir/clients/<name>/ instead of CWD.
				// Keeping output files co-located with token/pubkey prevents scatter
				// across arbitrary working directories.
				rscOutDir := filepath.Join(configDir, "clients", name)
				if err := os.MkdirAll(rscOutDir, 0700); err != nil {
					return fmt.Errorf("create client output dir %q: %w", rscOutDir, err)
				}
				rscFile := name + "-mikrotik.rsc"
				rscPath := filepath.Join(rscOutDir, rscFile)
				if err := os.WriteFile(rscPath, []byte(rsc), 0644); err != nil {
					return fmt.Errorf("write RouterOS script: %w", err)
				}

				tokenPath := filepath.Join(nd, "token")
				if showToken {
					_, _ = fmt.Fprintln(os.Stdout, token) // OK: gated behind --show-token flag
					logger := zerolog.New(os.Stderr).With().Timestamp().Logger()
					logger.Warn().
						Str("event", "show_token_flag").
						Str("command", "client prepare").
						Msg("token emitted to stdout; prefer 'cat <token-path>' for scripted retrieval")
				}
				tokenLine := fmt.Sprintf("Token saved to %s. Run 'cat %s' to retrieve.", tokenPath, tokenPath)
				fmt.Fprintf(os.Stderr, "MikroTik client %q prepared.\n\n%s\n\nRouterOS script written to: %s\n\nNext steps:\n  1. Copy %s to the MikroTik router\n  2. /import file-name=%s\n  3. mesh-ctl client init %s\n",
					name,
					tokenLine,
					rscPath,
					rscPath,
					rscFile,
					name,
				)

			default:
				return fmt.Errorf("unknown client type %q (must be linux or mikrotik)", client.Type)
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&useTraefik, "traefik", false, "Generate Traefik-compatible compose with labels (no host networking)")
	cmd.Flags().BoolVar(&showToken, "show-token", false, "print raw token to stdout (default: save to disk only)")
	cmd.Flags().StringVar(&imageFlag, "image", "", "Docker image reference (default: topology defaults.image.client, else ghcr.io/coonfuuseed-paandaa/awg-mesh-client:latest)")
	return cmd
}

func resolveClientTarget(topo *topology.Topology, client *topology.ClientNode) (string, error) {
	if len(client.Masters) == 0 {
		return "", fmt.Errorf("client %q has no masters", client.Name)
	}

	master := topo.FindMaster(client.Masters[0])
	if master == nil {
		return "", fmt.Errorf("master %q not found in topology", client.Masters[0])
	}

	return master.Host, nil
}

func resolveClientGRPCAddr(_ *topology.Topology, client *topology.ClientNode) string {
	return client.GRPCAddr()
}

// clientStorageRoot returns the topology-configured StorageRoot for a MikroTik client,
// or "" when unset (letting the generator apply the "docker" default).
func clientStorageRoot(client *topology.ClientNode) string {
	if client.Mikrotik != nil && client.Mikrotik.StorageRoot != "" {
		return client.Mikrotik.StorageRoot
	}
	return ""
}

var masterClientTunnelIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,12}$`)

func masterClientLegacyTunnelID(clientName string) string {
	return strings.TrimSpace(clientName)
}

func masterClientTunnelID(clientName string) string {
	trimmed := masterClientLegacyTunnelID(clientName)
	if trimmed == "" {
		return "cli-00000000"
	}
	if len(trimmed) <= 12 && masterClientTunnelIDPattern.MatchString(trimmed) {
		return trimmed
	}
	sum := sha256.Sum256([]byte(trimmed))
	return "cli-" + hex.EncodeToString(sum[:4])
}

func masterClientPreferredTunnelID(clientName string, tunnels []*proto.TunnelStatus) string {
	legacyName := masterClientLegacyTunnelID(clientName)
	boundedName := masterClientTunnelID(clientName)
	if legacyName == "" || legacyName == boundedName {
		return boundedName
	}
	for _, tunnel := range tunnels {
		if tunnel != nil && strings.TrimSpace(tunnel.GetName()) == legacyName {
			return legacyName
		}
	}
	return boundedName
}

func masterClientRemovalTunnelIDs(clientName string, tunnels []*proto.TunnelStatus) []string {
	legacyName := masterClientLegacyTunnelID(clientName)
	boundedName := masterClientTunnelID(clientName)
	seen := make(map[string]struct{}, 2)
	removals := make([]string, 0, 2)
	appendName := func(name string) {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return
		}
		if _, exists := seen[trimmed]; exists {
			return
		}
		seen[trimmed] = struct{}{}
		removals = append(removals, trimmed)
	}

	if len(tunnels) == 0 {
		appendName(legacyName)
		appendName(boundedName)
		return removals
	}

	present := make(map[string]struct{}, len(tunnels))
	for _, tunnel := range tunnels {
		if tunnel == nil {
			continue
		}
		present[strings.TrimSpace(tunnel.GetName())] = struct{}{}
	}
	if _, exists := present[legacyName]; exists {
		appendName(legacyName)
	}
	if _, exists := present[boundedName]; exists {
		appendName(boundedName)
	}
	return removals
}

func newClientInitCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "init [name]",
		Short: "Initialize client via gRPC",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			client := topo.FindClient(name)
			if client == nil {
				return fmt.Errorf("client %q not found in topology", name)
			}

			targetHost, err := resolveClientTarget(topo, client)
			if err != nil {
				return err
			}

			caCert, caKey, err := pkgtls.LoadCA(configDir)
			if err != nil {
				return fmt.Errorf("load CA: %w", err)
			}

			certPEM, keyPEM, err := pkgtls.IssueCert(caCert, caKey, client.Name, []string{targetHost})
			if err != nil {
				return fmt.Errorf("issue cert: %w", err)
			}

			caCertPEM, err := os.ReadFile(caPath(configDir))
			if err != nil {
				return fmt.Errorf("read CA cert: %w", err)
			}

			nd := nodeDir(configDir, name)
			token, err := loadToken(nd)
			if err != nil {
				return fmt.Errorf("load token: %w", err)
			}

			clientGRPC, err := grpcclient.NewClient(grpcclient.ClientConfig{
				Target:   resolveClientGRPCAddr(topo, client),
				Token:    token,
				Insecure: true,
			})
			if err != nil {
				return fmt.Errorf("create gRPC client: %w", err)
			}
			defer func() {
				if closeErr := clientGRPC.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client: %v\n", closeErr)
				}
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			resp, err := clientGRPC.Agent().Init(ctx, &proto.InitRequest{
				CaCert:   caCertPEM,
				NodeCert: certPEM,
				NodeKey:  keyPEM,
				Config: &proto.NodeConfig{
					Name:       client.Name,
					Mode:       "client",
					OverlayIp:  client.OverlayIP,
					ListenPort: 0,
				},
			})
			if err != nil {
				return fmt.Errorf("init RPC: %w", err)
			}
			if !resp.Success {
				return fmt.Errorf("init failed: %s", resp.Message)
			}

			pubkeyPath := filepath.Join(nd, "pubkey")
			if err := os.WriteFile(pubkeyPath, resp.NodePublicKey, 0644); err != nil {
				return fmt.Errorf("write client pubkey file %q: %w", pubkeyPath, err)
			}

			// F-005 fix: serialize the entire allocate+RPC+save block across
			// concurrent `mesh-ctl client init` invocations. Without this lock,
			// two parallel processes both see an empty allocator state, hand out
			// the SAME /30 transport subnet to different clients, and the
			// last-writer-wins save loses one client's allocation. Lock scope
			// covers loadOrCreateAllocator → alloc.Allocate × N → AddTunnel RPC →
			// saveTransportState so allocations are atomically reserved on disk
			// before the next process loads the file.
			lockPath := filepath.Join(configDir, "transport-state.lock")
			lockRelease, lockErr := acquireFileLockWithRetry(lockPath, 180*time.Second)
			if lockErr != nil {
				return fmt.Errorf("client %q: acquire admin transport-state lock: %w", name, lockErr)
			}
			defer lockRelease()

			alloc, err := loadOrCreateAllocator(configDir, topo)
			if err != nil {
				return fmt.Errorf("load transport allocator: %w", err)
			}

			mastersConnected := 0
			parsedRanges := make([]topology.Range, 0, len(topo.Overlay.Ranges))
			for _, nr := range topo.Overlay.Ranges {
				if r, rErr := topology.ParseRange(nr); rErr == nil {
					parsedRanges = append(parsedRanges, r)
				} else {
					fmt.Fprintf(os.Stderr, "warning: failed to parse topology range %q: %v\n", nr, rErr)
				}
			}

			for _, masterName := range client.Masters {
				master := topo.FindMaster(masterName)
				if master == nil {
					fmt.Fprintf(os.Stderr, "warning: master %q not found in topology, skipping\n", masterName)
					continue
				}

				var masterBalancerIP string
				if masterAddr, parseErr := netip.ParseAddr(master.OverlayIP); parseErr == nil {
					if bip := topology.BalancerIPForAddr(parsedRanges, masterAddr); bip.IsValid() {
						masterBalancerIP = bip.String()
					}
				} else {
					fmt.Fprintf(os.Stderr, "warning: failed to parse master overlay IP %q for master %q: %v\n", master.OverlayIP, master.Name, parseErr)
				}

				allocation, err := alloc.Allocate(master.Name, client.Name)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: allocate transport for master %q and client %q failed: %v\n", master.Name, name, err)
					continue
				}

				masterPubkey, err := readAdminPubkeyRaw(configDir, master.Name)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: read master %q pubkey: %v\n", master.Name, err)
					continue
				}
				if len(masterPubkey) == 0 {
					fmt.Fprintf(os.Stderr, "warning: master %q pubkey is empty\n", master.Name)
					continue
				}

				masterToken, err := loadToken(nodeDir(configDir, master.Name))
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: load token for master %q: %v\n", master.Name, err)
					continue
				}

				masterClient, err := grpcclient.NewClient(grpcclient.ClientConfig{
					Target:   master.GRPCAddr(),
					Token:    masterToken,
					Insecure: true,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: connect to master %q: %v\n", master.Name, err)
					continue
				}

				masterCtx, masterCancel := context.WithTimeout(context.Background(), 30*time.Second)
				tunnelList, listErr := masterClient.Agent().ListTunnels(masterCtx, &proto.Empty{})
				masterCancel()
				if listErr != nil {
					fmt.Fprintf(os.Stderr, "warning: list tunnels on master %q failed: %v\n", master.Name, listErr)
					if closeErr := masterClient.Close(); closeErr != nil {
						fmt.Fprintf(os.Stderr, "warning: close grpc client for master %q: %v\n", master.Name, closeErr)
					}
					continue
				}
				masterTunnelName := masterClientPreferredTunnelID(client.Name, tunnelList.GetTunnels())

				// Client AllowedIPs: transport subnet + client overlay IP.
				// Without these, master WireGuard drops client packets with
				// overlay source IPs (not in peer AllowedIPs).
				clientAllowedIPs := []string{
					allocation.Subnet.String(),
					client.OverlayIP + "/32",
				}

				masterCtx, masterCancel = context.WithTimeout(context.Background(), 30*time.Second)
				addResp, addErr := masterClient.Agent().AddTunnel(masterCtx, &proto.AddTunnelRequest{
					Name:                masterTunnelName,
					EndpointHost:        "",
					OverlayIp:           client.OverlayIP,
					BalancerIp:          "",
					PeerPublicKey:       resp.NodePublicKey,
					Weight:              1,
					TransportSubnet:     allocation.Subnet.String(),
					MasterTransportIp:   allocation.MasterIP.String(),
					EndpointTransportIp: allocation.EndpointIP.String(),
					AllowedIps:          clientAllowedIPs,
				})
				masterCancel()
				if closeErr := masterClient.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client for master %q: %v\n", master.Name, closeErr)
				}

				if addErr != nil {
					fmt.Fprintf(os.Stderr, "warning: add tunnel on master %q failed: %v\n", master.Name, addErr)
					continue
				}
				if addResp == nil || !addResp.Success {
					fmt.Fprintf(os.Stderr, "warning: add tunnel on master %q failed: [RPC failure]\n", master.Name)
					continue
				}

				clientPeerClient, err := grpcclient.NewClient(grpcclient.ClientConfig{
					Target:   resolveClientGRPCAddr(topo, client),
					Insecure: true,
					Token:    token,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: connect to client %q for add peer: %v\n", name, err)
					continue
				}

				// Use the actual listen port from the master's per-tunnel WG interface
				// (not master.ListenPort which is the config default, not the ephemeral port).
				peerHost := master.PeerHost
				if peerHost == "" {
					peerHost = master.Host
				}
				tunnelEndpoint := fmt.Sprintf("%s:%d", peerHost, addResp.ListenPort)

				peerCtx, peerCancel := context.WithTimeout(context.Background(), 30*time.Second)
				peerResp, peerErr := clientPeerClient.Agent().AddPeer(peerCtx, &proto.AddPeerRequest{
					PublicKey:           masterPubkey,
					AllowedIps:          []string{"0.0.0.0/0"},
					EndpointHost:        master.Name + "|" + tunnelEndpoint,
					PersistentKeepalive: 25,
					TransportSubnet:     allocation.Subnet.String(),
					LocalTransportIp:    allocation.EndpointIP.String(),
					PeerTransportIp:     allocation.MasterIP.String(),
					BalancerIp:          masterBalancerIP,
					ExtraRoutes:         []string{topo.Overlay.Space},
				})
				peerCancel()
				if closeErr := clientPeerClient.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client for client %q: %v\n", name, closeErr)
				}

				if peerErr != nil {
					fmt.Fprintf(os.Stderr, "warning: add peer on client %q for master %q failed: %v\n", name, master.Name, peerErr)
					continue
				}
				if peerResp == nil || !peerResp.Success {
					fmt.Fprintf(os.Stderr, "warning: add peer on client %q for master %q failed: [RPC failure]\n", name, master.Name)
					continue
				}

				fmt.Printf("Added peer on client %q for master %q.\n", name, master.Name)
				mastersConnected++
			}

			if mastersConnected == 0 {
				return fmt.Errorf("client %q: no masters connected — initialization incomplete (check master availability)", name)
			}

			if err := saveTransportState(alloc, configDir); err != nil {
				return fmt.Errorf("save transport state: %w", err)
			}

			if err := saveClientInitState(nodeDir(configDir, name), node.ClientState{
				OverlayIP:    client.OverlayIP,
				OverlaySpace: topo.Overlay.Space,
			}); err != nil {
				return fmt.Errorf("save client init state: %w", err)
			}

			// F-005 fix: partial-master-failure must surface as non-zero exit so
			// orchestration scripts (dpext fixture, autopilot) detect the
			// degraded onboarding instead of seeing exit 0 and proceeding into
			// data-plane assertions that will fail downstream. Print partial vs
			// full status separately so stdout matches the actual outcome.
			if mastersConnected < len(client.Masters) {
				fmt.Printf("Client %q partially initialized: %d/%d masters connected.\nPublic key: %s\n",
					name, mastersConnected, len(client.Masters), hex.EncodeToString(resp.NodePublicKey))
				return fmt.Errorf("client %q: only %d/%d masters connected — partial init (re-run after fixing failed masters or run `mesh-ctl reconcile`)",
					name, mastersConnected, len(client.Masters))
			}
			fmt.Printf("Client %q initialized: %d/%d masters connected.\nPublic key: %s\n",
				name, mastersConnected, len(client.Masters), hex.EncodeToString(resp.NodePublicKey))
			return nil
		},
	}
}

func newClientRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove [name]",
		Short: "Remove a client from the mesh",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			topo, err := topology.LoadTopology(topologyPath)
			if err != nil {
				return fmt.Errorf("load topology %q: %w", topologyPath, err)
			}

			client := topo.FindClient(name)
			if client == nil {
				return fmt.Errorf("client %q not found in topology", name)
			}

			for _, masterName := range client.Masters {
				master := topo.FindMaster(masterName)
				if master == nil {
					fmt.Fprintf(os.Stderr, "warning: master %q not found in topology, skipping\n", masterName)
					continue
				}

				masterToken, err := loadToken(nodeDir(configDir, master.Name))
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: cannot load token for master %q: %v\n", master.Name, err)
					continue
				}

				masterClient, err := grpcclient.NewClient(grpcclient.ClientConfig{
					Target:   master.GRPCAddr(),
					Token:    masterToken,
					Insecure: true,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: cannot connect to master %q: %v\n", master.Name, err)
					continue
				}

				masterCtx, masterCancel := context.WithTimeout(context.Background(), 30*time.Second)
				tunnelList, listErr := masterClient.Agent().ListTunnels(masterCtx, &proto.Empty{})
				masterCancel()
				if listErr != nil {
					fmt.Fprintf(os.Stderr, "warning: list tunnels on master %q failed: %v\n", master.Name, listErr)
					if closeErr := masterClient.Close(); closeErr != nil {
						fmt.Fprintf(os.Stderr, "warning: close grpc client for master %q: %v\n", master.Name, closeErr)
					}
					continue
				}

				removedAny := false
				for _, tunnelName := range masterClientRemovalTunnelIDs(client.Name, tunnelList.GetTunnels()) {
					masterCtx, masterCancel = context.WithTimeout(context.Background(), 30*time.Second)
					removeResp, removeErr := masterClient.Agent().RemoveTunnel(masterCtx, &proto.RemoveTunnelRequest{
						Name: tunnelName,
					})
					masterCancel()

					if removeErr != nil {
						fmt.Fprintf(os.Stderr, "warning: remove tunnel %q from master %q failed: %v\n", tunnelName, master.Name, removeErr)
						continue
					}
					if removeResp == nil || !removeResp.Success {
						fmt.Fprintf(os.Stderr, "warning: remove tunnel %q from master %q failed: %s\n", tunnelName, master.Name, "[RPC failure]")
						continue
					}
					removedAny = true
					fmt.Printf("Removed tunnel %q for client %q from master %q.\n", tunnelName, client.Name, master.Name)
				}

				if closeErr := masterClient.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: close grpc client for master %q: %v\n", master.Name, closeErr)
				}
				if !removedAny {
					fmt.Fprintf(os.Stderr, "warning: no tunnels removed for client %q from master %q\n", client.Name, master.Name)
				}
			}

			fmt.Printf("Client removed from all masters. Manual cleanup may be needed on client host.\n")
			return nil
		},
	}
}
