package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/coonfuuseed-paandaa/awg-mesh/cmd/mesh-ctl/internal/adminstate"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/awggen"
	grpcclient "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/rotation"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
)

const (
	defaultRotatePreset    = "Balanced"
	defaultRotateAgentPort = 9090
	rotateTimeout          = 30 * time.Second
)

type rotateOptions struct {
	tier       int
	endpoint   string
	preset     string
	familyName string
}

func newRotateCommand() *cobra.Command {
	options := rotateOptions{
		preset: defaultRotatePreset,
	}

	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Rotate AWG parameters for an endpoint across masters",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRotateCommand(options)
		},
	}

	cmd.Flags().IntVar(&options.tier, "tier", 0, "Rotation tier (1, 2, or 3)")
	cmd.Flags().StringVar(&options.endpoint, "endpoint", "", "Endpoint name in topology")
	cmd.Flags().StringVar(&options.preset, "preset", defaultRotatePreset, "AWG preset name")
	cmd.Flags().StringVar(&options.familyName, "family", "", "Optional protocol family name")

	return cmd
}

func runRotateCommand(options rotateOptions) error {
	validatedOptions, err := validateRotateOptions(options)
	if err != nil {
		return err
	}

	topo, err := topology.LoadTopology(topologyPath)
	if err != nil {
		return fmt.Errorf("load topology: %w", err)
	}

	endpoint, masters, err := resolveRotationMasters(topo, validatedOptions.endpoint)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), rotateTimeout)
	defer cancel()

	switch validatedOptions.tier {
	case 1:
		params, err := buildRotationParams(validatedOptions.preset, validatedOptions.familyName)
		if err != nil {
			return err
		}
		return executeTier1Rotation(ctx, endpoint, masters, params)
	case 2:
		params, err := buildRotationParams(validatedOptions.preset, validatedOptions.familyName)
		if err != nil {
			return err
		}
		return executeTier2Rotation(ctx, endpoint, masters, params)
	case 3:
		return executeTier3Rotation(ctx, endpoint, masters)
	default:
		return fmt.Errorf("invalid tier %d: must be 1, 2, or 3", validatedOptions.tier)
	}
}

func validateRotateOptions(options rotateOptions) (rotateOptions, error) {
	trimmedEndpoint := strings.TrimSpace(options.endpoint)
	if trimmedEndpoint == "" {
		return rotateOptions{}, fmt.Errorf("--endpoint is required")
	}

	trimmedPreset := strings.TrimSpace(options.preset)
	if trimmedPreset == "" {
		return rotateOptions{}, fmt.Errorf("--preset must not be empty")
	}

	trimmedFamily := strings.TrimSpace(options.familyName)
	if options.tier < 1 || options.tier > 3 {
		return rotateOptions{}, fmt.Errorf("--tier must be one of 1, 2, or 3")
	}

	return rotateOptions{
		tier:       options.tier,
		endpoint:   trimmedEndpoint,
		preset:     trimmedPreset,
		familyName: trimmedFamily,
	}, nil
}

func resolveRotationMasters(topo *topology.Topology, endpointName string) (*topology.EndpointNode, []topology.MasterNode, error) {
	if topo == nil {
		return nil, nil, fmt.Errorf("topology is required")
	}

	endpoint := topo.FindEndpoint(endpointName)
	if endpoint == nil {
		return nil, nil, fmt.Errorf("endpoint %q not found", endpointName)
	}

	masters := make([]topology.MasterNode, 0, len(topo.Masters))
	for _, master := range topo.Masters {
		if containsName(master.Endpoints, endpoint.Name) {
			masters = append(masters, master)
		}
	}

	if len(masters) == 0 {
		return nil, nil, fmt.Errorf("no masters found for endpoint %q", endpoint.Name)
	}

	return endpoint, masters, nil
}

func buildRotationParams(presetName, familyName string) (*awggen.Params, error) {
	preset, err := awggen.GetPreset(presetName)
	if err != nil {
		return nil, fmt.Errorf("load preset %q: %w", presetName, err)
	}

	var family *awggen.ProtocolFamily
	if familyName != "" {
		family, err = awggen.GetFamily(familyName)
		if err != nil {
			return nil, fmt.Errorf("load family %q: %w", familyName, err)
		}
	}

	params, err := awggen.GenerateParams(preset, family)
	if err != nil {
		return nil, fmt.Errorf("generate rotation params: %w", err)
	}
	if params == nil {
		return nil, fmt.Errorf("generate rotation params: empty params")
	}

	return params, nil
}

func executeTier1Rotation(ctx context.Context, endpoint *topology.EndpointNode, masters []topology.MasterNode, params *awggen.Params) error {
	rotator := rotation.NewTier1Rotation()
	failures := make([]string, 0)

	for _, master := range masters {
		client, err := connectMasterAgent(master)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: tier 1 rotation failed: %v\n", master.Name, err)
			failures = append(failures, fmt.Sprintf("%s: %v", master.Name, err))
			continue
		}

		executeErr := rotator.Execute(ctx, client.Agent(), endpoint.Name, params)
		if executeErr != nil {
			fmt.Fprintf(os.Stderr, "%s: tier 1 rotation failed: %v\n", master.Name, executeErr)
			failures = append(failures, fmt.Sprintf("%s: %v", master.Name, executeErr))
		} else {
			fmt.Printf("%s: tier 1 rotation succeeded\n", master.Name)
		}

		if closeErr := client.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: close %s: %v\n", master.Name, closeErr)
		}
	}

	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("tier 1 rotation failed: %s", strings.Join(failures, "; "))
	}

	return nil
}

func executeTier2Rotation(ctx context.Context, endpoint *topology.EndpointNode, masters []topology.MasterNode, params *awggen.Params) error {
	agents := make(map[string]proto.AwgAgentClient, len(masters))
	clients := make(map[string]*grpcclient.Client, len(masters))
	connectionFailures := make([]string, 0)

	for _, master := range masters {
		client, err := connectMasterAgent(master)
		if err != nil {
			connectionFailures = append(connectionFailures, fmt.Sprintf("%s: %v", master.Name, err))
			continue
		}

		clients[master.Name] = client
		agents[master.Name] = client.Agent()
	}
	defer closeMasterClients(clients)

	if len(connectionFailures) > 0 {
		sort.Strings(connectionFailures)
		return fmt.Errorf("tier 2 rotation connection failed: %s", strings.Join(connectionFailures, "; "))
	}

	if err := rotation.NewTier2Rotation().Execute(ctx, agents, endpoint.Name, params); err != nil {
		fmt.Fprintf(os.Stderr, "tier 2 rotation failed: %v\n", err)
		return fmt.Errorf("tier 2 execute: %w", err)
	}

	fmt.Printf("tier 2 rotation succeeded on %d masters\n", len(agents))
	return nil
}

// tier3MasterResult tracks the final per-master state for the structured stderr table.
// STATUS values:
//   - ROTATED       — UpdateTunnelPeer succeeded and all masters succeeded (no rollback)
//   - FAILED        — UpdateTunnelPeer failed (master was never rotated)
//   - REVERTED      — UpdateTunnelPeer succeeded, then rollback also succeeded
//   - REVERT_FAILED — UpdateTunnelPeer succeeded, rollback failed (manual recovery needed)
type tier3MasterResult struct {
	name   string
	status string
	detail string
}

// executeTier3Rotation implements the 4-party coordinated keypair rotation:
//
//  1. Read old admin-state pubkey for rollback.
//  2. Generate fresh curve25519 keypair on CLI.
//  3. Push new private key to endpoint via RotateKeypair RPC.
//  4. Fan-out UpdateTunnelPeer to every master; best-effort revert on failure.
//  5. Commit new pubkey to admin-state.
//
// CRITICAL: there is NO idempotency short-circuit. Every invocation generates
// a fresh keypair and executes all four steps unconditionally (FR-9 / DD-4).
func executeTier3Rotation(ctx context.Context, endpoint *topology.EndpointNode, masters []topology.MasterNode) error {
	// Step 0: Read old admin-state pubkey.
	// Needed as the rollback target if master fan-out partially fails.
	// Admin-state stores pubkeys in WireGuard canonical base64 (wg.Key.String()).
	// NEVER use hex.DecodeString — admin-state is NOT hex-encoded.
	store := adminstate.NewStore(configDir)
	oldPubKeyStr, err := store.GetPubkey(endpoint.Name)
	if err != nil {
		return fmt.Errorf("read admin-state pubkey for %q: %w", endpoint.Name, err)
	}

	var oldPubKey wg.Key
	if oldPubKeyStr != "" {
		oldPubKey, err = wg.ParseKey(oldPubKeyStr)
		if err != nil {
			return fmt.Errorf("admin-state pubkey for %q is corrupt (parse failed): %w", endpoint.Name, err)
		}
	}

	// Step 1: Generate fresh curve25519 keypair.
	privateKey, err := wg.GeneratePrivateKey()
	if err != nil {
		return fmt.Errorf("generate keypair: %w", err)
	}
	publicKey := privateKey.PublicKey()

	// Step 2: Push new private key to endpoint via RotateKeypair RPC.
	// On any error from RotateKeypair: abort immediately — masters have NOT been touched.
	endpointClient, err := connectEndpointAgent(*endpoint)
	if err != nil {
		return fmt.Errorf("connect to endpoint %q: %w", endpoint.Name, err)
	}
	defer func() {
		if closeErr := endpointClient.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: close endpoint %s: %v\n", endpoint.Name, closeErr)
		}
	}()

	rotateResp, err := endpointClient.Agent().RotateKeypair(ctx, &proto.RotateKeypairRequest{
		PrivateKey: privateKey[:],
		TunnelName: endpoint.Name,
	})
	if err != nil {
		return fmt.Errorf("rotate keypair on endpoint %q: %w", endpoint.Name, err)
	}

	// Sanity check: endpoint-returned public key MUST match our locally-derived key.
	// If they differ, the endpoint derived from a different private key — abort.
	if !bytes.Equal(rotateResp.NewPublicKey, publicKey[:]) {
		return fmt.Errorf("endpoint %q returned mismatched public key (derivation mismatch — do not proceed)", endpoint.Name)
	}

	// Step 3: Fan-out UpdateTunnelPeer to every master.
	results := make([]tier3MasterResult, 0, len(masters))
	rotatedMasters := make([]topology.MasterNode, 0, len(masters))
	anyFailure := false

	for _, master := range masters {
		mClient, err := connectMasterAgent(master)
		if err != nil {
			results = append(results, tier3MasterResult{
				name:   master.Name,
				status: "FAILED",
				detail: fmt.Sprintf("connect: %v", err),
			})
			anyFailure = true
			continue
		}

		_, rpcErr := mClient.Agent().UpdateTunnelPeer(ctx, &proto.UpdateTunnelPeerRequest{
			Name:          endpoint.Name,
			PeerPublicKey: publicKey[:],
		})
		if closeErr := mClient.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: close master %s: %v\n", master.Name, closeErr)
		}

		if rpcErr != nil {
			results = append(results, tier3MasterResult{
				name:   master.Name,
				status: "FAILED",
				detail: rpcErr.Error(),
			})
			anyFailure = true
		} else {
			results = append(results, tier3MasterResult{
				name:   master.Name,
				status: "ROTATED",
				detail: "",
			})
			rotatedMasters = append(rotatedMasters, master)
		}
	}

	if anyFailure {
		// Best-effort rollback: revert already-rotated masters to oldPubKey.
		// The endpoint private key is NOT rolled back (no client-side copy of old private key;
		// operator must run 'mesh-ctl reconcile <endpoint>' to restore consistency — NFR-7 / DD-3).
		for i := range results {
			if results[i].status != "ROTATED" {
				continue
			}
			// Find the master struct for this result entry.
			var revertMaster *topology.MasterNode
			for j := range rotatedMasters {
				if rotatedMasters[j].Name == results[i].name {
					revertMaster = &rotatedMasters[j]
					break
				}
			}
			if revertMaster == nil {
				results[i].status = "REVERT_FAILED"
				results[i].detail = "internal: master struct not found during rollback"
				continue
			}

			mClient, connErr := connectMasterAgent(*revertMaster)
			if connErr != nil {
				results[i].status = "REVERT_FAILED"
				results[i].detail = fmt.Sprintf("revert connect: %v", connErr)
				continue
			}

			// oldPubKey is the original public key parsed from admin-state (base64 → wg.Key).
			// Passing oldPubKey[:] restores the peer entry to the pre-rotation state.
			_, revertErr := mClient.Agent().UpdateTunnelPeer(ctx, &proto.UpdateTunnelPeerRequest{
				Name:          endpoint.Name,
				PeerPublicKey: oldPubKey[:],
			})
			if closeErr := mClient.Close(); closeErr != nil {
				fmt.Fprintf(os.Stderr, "warning: close master %s (revert): %v\n", revertMaster.Name, closeErr)
			}

			if revertErr != nil {
				results[i].status = "REVERT_FAILED"
				results[i].detail = fmt.Sprintf("revert rpc: %v", revertErr)
			} else {
				results[i].status = "REVERTED"
				results[i].detail = ""
			}
		}

		printTier3Table(results)

		// Count failures for the error summary.
		failCount := 0
		for _, r := range results {
			if r.status == "FAILED" || r.status == "REVERT_FAILED" {
				failCount++
			}
		}
		fmt.Fprintf(os.Stderr, "tier 3 rotation FAILED — run 'mesh-ctl reconcile %s' to force-sync cluster\n", endpoint.Name)
		return fmt.Errorf("tier 3 rotation partial failure: %d master(s) in unrecoverable state", failCount)
	}

	// Step 4: All masters confirmed — commit admin-state atomically.
	// Admin-state is the LAST step (NFR-3 / DD-5): file reflects actual cluster state.
	newPubKeyStr := publicKey.String()
	if _, err := store.SetPubkey(endpoint.Name, func(_ string) (string, error) {
		return newPubKeyStr, nil
	}); err != nil {
		// Masters already committed. Admin-state write failed.
		// Surface as a cluster-inconsistent state; operator must run 'mesh-ctl reconcile'.
		printTier3Table(results)
		fmt.Fprintf(os.Stderr, "tier 3 rotation: masters updated but admin-state commit failed — run 'mesh-ctl reconcile %s'\n", endpoint.Name)
		return fmt.Errorf("commit admin-state: %w", err)
	}

	printTier3Table(results)
	fmt.Printf("tier 3 rotation complete: endpoint %q pubkey rotated (%d/%d masters)\n",
		endpoint.Name, len(masters), len(masters))
	return nil
}

// printTier3Table emits the structured NAME/STATUS/DETAIL table to stderr.
// One row per master, final status only — never duplicate rows.
// STATUS values: ROTATED, FAILED, REVERTED, REVERT_FAILED.
func printTier3Table(results []tier3MasterResult) {
	w := tabwriter.NewWriter(os.Stderr, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tDETAIL")
	for _, r := range results {
		detail := r.detail
		if detail == "" {
			detail = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.name, r.status, detail)
	}
	_ = w.Flush()
}

// connectEndpointAgent dials the gRPC agent on an endpoint node.
// Mirrors connectMasterAgent but targets the endpoint's gRPC address
// and loads the endpoint's token from the admin-state config dir.
func connectEndpointAgent(endpoint topology.EndpointNode) (*grpcclient.Client, error) {
	token, err := loadToken(nodeDir(configDir, endpoint.Name))
	if err != nil {
		return nil, fmt.Errorf("load token for endpoint %q: %w", endpoint.Name, err)
	}

	client, err := grpcclient.NewClient(grpcclient.ClientConfig{
		Target:     endpoint.GRPCAddr(),
		CACertPath: caPath(configDir),
		Token:      token,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to endpoint %q: %w", endpoint.Name, err)
	}

	return client, nil
}

// masterGRPCTarget returns the host:port gRPC dial target for a master node,
// applying the default port when master.GRPCPort is zero.
func masterGRPCTarget(master topology.MasterNode) string {
	port := master.GRPCPort
	if port == 0 {
		port = defaultRotateAgentPort
	}
	return net.JoinHostPort(master.Host, strconv.Itoa(port))
}

func connectMasterAgent(master topology.MasterNode) (*grpcclient.Client, error) {
	token, err := loadToken(nodeDir(configDir, master.Name))
	if err != nil {
		return nil, fmt.Errorf("load token for %q: %w", master.Name, err)
	}

	client, err := grpcclient.NewClient(grpcclient.ClientConfig{
		Target:     masterGRPCTarget(master),
		CACertPath: caPath(configDir),
		Token:      token,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to master %q: %w", master.Name, err)
	}

	return client, nil
}

func closeMasterClients(clients map[string]*grpcclient.Client) {
	names := make([]string, 0, len(clients))
	for name := range clients {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		client := clients[name]
		if client == nil {
			continue
		}
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: close %s: %v\n", name, err)
		}
	}
}
