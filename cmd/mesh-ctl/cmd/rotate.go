package cmd

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/coonfuuseed-paandaa/awg-mesh/cmd/mesh-ctl/internal/adminstate"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/awggen"
	grpcclient "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/rotation"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	controlpb "github.com/coonfuuseed-paandaa/awg-mesh/proto/control_plane"
	"github.com/spf13/cobra"
)

const (
	defaultRotatePreset    = "Balanced"
	defaultRotateAgentPort = 9090
	rotateTimeout          = 30 * time.Second
)

type rotateOptions struct {
	tier         int
	endpoint     string
	preset       string
	familyName   string
	meshWide     bool
	controlPlane string
	configDir    string
	applyDelay   time.Duration
	stdout       io.Writer
}

func newRotateCommand() *cobra.Command {
	options := rotateOptions{
		preset:     defaultRotatePreset,
		applyDelay: rotation.DefaultApplyLeadTime,
	}

	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Rotate AWG parameters for an endpoint or the mesh",
		RunE: func(cmd *cobra.Command, args []string) error {
			options.configDir = configDir
			return runRotateCommand(options)
		},
	}

	cmd.Flags().IntVar(&options.tier, "tier", 0, "Rotation tier (1, 2, or 3)")
	cmd.Flags().StringVar(&options.endpoint, "endpoint", "", "Endpoint name in topology")
	cmd.Flags().StringVar(&options.preset, "preset", defaultRotatePreset, "AWG preset name")
	cmd.Flags().StringVar(&options.familyName, "family", "", "Optional protocol family name")
	cmd.Flags().BoolVar(&options.meshWide, "mesh-wide", false, "Rotate every mesh-internal node through the control plane")
	cmd.Flags().StringVar(&options.controlPlane, "control-plane", "", "Control-plane gRPC address for --mesh-wide")
	cmd.Flags().DurationVar(&options.applyDelay, "apply-delay", rotation.DefaultApplyLeadTime, "Mesh-wide apply delay")

	return cmd
}

func runRotateCommand(options rotateOptions) error {
	validatedOptions, err := validateRotateOptions(options)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), rotateCommandTimeout(validatedOptions))
	defer cancel()

	if validatedOptions.meshWide {
		return executeMeshWideRotation(ctx, validatedOptions)
	}

	topo, err := topology.LoadTopology(topologyPath)
	if err != nil {
		return fmt.Errorf("load topology: %w", err)
	}

	endpoint, masters, err := resolveRotationMasters(topo, validatedOptions.endpoint)
	if err != nil {
		return err
	}

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
	trimmedPreset := strings.TrimSpace(options.preset)
	if trimmedPreset == "" {
		return rotateOptions{}, fmt.Errorf("--preset must not be empty")
	}
	trimmedFamily := strings.TrimSpace(options.familyName)
	trimmedControlPlane := strings.TrimSpace(options.controlPlane)
	if options.tier < 1 || options.tier > 3 {
		return rotateOptions{}, fmt.Errorf("--tier must be one of 1, 2, or 3")
	}
	if options.meshWide {
		if trimmedEndpoint != "" {
			return rotateOptions{}, fmt.Errorf("--endpoint cannot be used with --mesh-wide")
		}
		if trimmedControlPlane == "" {
			return rotateOptions{}, fmt.Errorf("--control-plane is required with --mesh-wide")
		}
		if options.applyDelay <= 0 {
			return rotateOptions{}, fmt.Errorf("--apply-delay must be greater than zero")
		}
	} else {
		if trimmedEndpoint == "" {
			return rotateOptions{}, fmt.Errorf("--endpoint is required")
		}
		if trimmedControlPlane != "" {
			return rotateOptions{}, fmt.Errorf("--control-plane requires --mesh-wide")
		}
	}

	return rotateOptions{
		tier:         options.tier,
		endpoint:     trimmedEndpoint,
		preset:       trimmedPreset,
		familyName:   trimmedFamily,
		meshWide:     options.meshWide,
		controlPlane: trimmedControlPlane,
		configDir:    options.configDir,
		applyDelay:   options.applyDelay,
		stdout:       options.stdout,
	}, nil
}

func rotateCommandTimeout(options rotateOptions) time.Duration {
	if options.meshWide {
		return rotateTimeout + options.applyDelay
	}
	return rotateTimeout
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

func executeMeshWideRotation(ctx context.Context, options rotateOptions) error {
	params, err := buildRotationParams(options.preset, options.familyName)
	if err != nil {
		return err
	}
	tier, err := rotation.NormalizeTier(strconv.Itoa(options.tier))
	if err != nil {
		return err
	}
	conn, err := newControlPlaneAdminConn(options.controlPlane, options.configDir)
	if err != nil {
		return fmt.Errorf("connect control-plane %q: %w", options.controlPlane, err)
	}
	defer func() { _ = conn.Close() }()

	stream, err := controlpb.NewControlPlaneClient(conn).RotateAWGParamsMeshWide(ctx)
	if err != nil {
		return fmt.Errorf("open mesh-wide rotation stream: %w", err)
	}
	if err := stream.Send(&controlpb.RotateRequest{
		Tier:              tier,
		NewParams:         controlPlaneParamsFromAWG(params),
		ApplyAtUnixMicros: time.Now().UTC().Add(options.applyDelay).UnixMicro(),
	}); err != nil {
		return fmt.Errorf("send mesh-wide rotation request: %w", err)
	}
	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("close mesh-wide rotation request stream: %w", err)
	}

	results, recvErr := collectMeshWideRotationResponses(stream)
	if len(results) > 0 {
		if err := printMeshWideRotationRows(rotateOutput(options), results); err != nil {
			return err
		}
	}
	if recvErr != nil {
		return fmt.Errorf("mesh-wide rotation failed: %w", recvErr)
	}
	return nil
}

func controlPlaneParamsFromAWG(params *awggen.Params) *controlpb.AWGParamsV2 {
	if params == nil {
		return nil
	}
	return &controlpb.AWGParamsV2{
		Jc:   int32(params.Jc),
		Jmin: int32(params.Jmin),
		Jmax: int32(params.Jmax),
		S1:   int32(params.S1),
		S2:   int32(params.S2),
		H1:   uint32(params.H1),
		H2:   uint32(params.H2),
		H3:   uint32(params.H3),
		H4:   uint32(params.H4),
		I1:   []byte(params.I1),
		I2:   []byte(params.I2),
		I3:   []byte(params.I3),
		I4:   []byte(params.I4),
		I5:   []byte(params.I5),
	}
}

func collectMeshWideRotationResponses(stream controlpb.ControlPlane_RotateAWGParamsMeshWideClient) ([]*controlpb.RotateResponse, error) {
	results := make([]*controlpb.RotateResponse, 0)
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return results, nil
		}
		if err != nil {
			return results, err
		}
		results = append(results, resp)
	}
}

func printMeshWideRotationRows(out io.Writer, results []*controlpb.RotateResponse) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "NODE\tACK\tERROR"); err != nil {
		return err
	}
	for _, result := range results {
		detail := result.GetError()
		if detail == "" {
			detail = "-"
		}
		if _, err := fmt.Fprintf(w, "%s\t%t\t%s\n", result.GetNodeName(), result.GetAck(), detail); err != nil {
			return err
		}
	}
	return w.Flush()
}

func rotateOutput(options rotateOptions) io.Writer {
	if options.stdout != nil {
		return options.stdout
	}
	return os.Stdout
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
	// Admin-state stores pubkeys as 64 hex chars (see endpoint.go's
	// `newPubKeyHex := hex.EncodeToString(resp.NodePublicKey)` + SetPubkey).
	store := adminstate.NewStore(configDir)
	oldPubKeyStr, err := store.GetPubkey(endpoint.Name)
	if err != nil {
		return fmt.Errorf("read admin-state pubkey for %q: %w", endpoint.Name, err)
	}

	// Guard: tier-3 rotation requires a prior `mesh-ctl endpoint init`. Without
	// admin-state there is no old key for masters to Remove; UpdateTunnelPeer
	// would be called with a zero-value pubkey and any rollback attempt would
	// target the zero key — corrupting cluster state. Operator must run init
	// first.
	if strings.TrimSpace(oldPubKeyStr) == "" {
		return fmt.Errorf("tier-3 rotation requires an initialized endpoint: no admin-state pubkey for %q (run `mesh-ctl endpoint init %s` first)", endpoint.Name, endpoint.Name)
	}

	var oldPubKey wg.Key
	oldBytes, decErr := hex.DecodeString(strings.TrimSpace(oldPubKeyStr))
	if decErr != nil {
		return fmt.Errorf("admin-state pubkey for %q is corrupt (hex decode failed): %w", endpoint.Name, decErr)
	}
	if len(oldBytes) != 32 {
		return fmt.Errorf("admin-state pubkey for %q has wrong length %d (want 32)", endpoint.Name, len(oldBytes))
	}
	copy(oldPubKey[:], oldBytes)

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

			// oldPubKey is the original public key parsed from admin-state (hex → wg.Key).
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

		if tableErr := printTier3Table(results); tableErr != nil {
			fmt.Fprintf(os.Stderr, "tier 3 rotation: failed to write results table: %v\n", tableErr)
		}

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
	// Admin-state format is 64 hex chars (see endpoint.go's newPubKeyHex pattern).
	newPubKeyStr := hex.EncodeToString(publicKey[:])
	if _, err := store.SetPubkey(endpoint.Name, func(_ string) (string, error) {
		return newPubKeyStr, nil
	}); err != nil {
		// Masters already committed. Admin-state write failed.
		// Surface as a cluster-inconsistent state; operator must run 'mesh-ctl reconcile'.
		if tableErr := printTier3Table(results); tableErr != nil {
			fmt.Fprintf(os.Stderr, "tier 3 rotation: failed to write results table: %v\n", tableErr)
		}
		fmt.Fprintf(os.Stderr, "tier 3 rotation: masters updated but admin-state commit failed — run 'mesh-ctl reconcile %s'\n", endpoint.Name)
		return fmt.Errorf("commit admin-state: %w", err)
	}

	if tableErr := printTier3Table(results); tableErr != nil {
		fmt.Fprintf(os.Stderr, "tier 3 rotation: failed to write results table: %v\n", tableErr)
	}
	fmt.Printf("tier 3 rotation complete: endpoint %q pubkey rotated (%d/%d masters)\n",
		endpoint.Name, len(masters), len(masters))
	return nil
}

// printTier3Table emits the structured NAME/STATUS/DETAIL table to stderr.
// One row per master, final status only — never duplicate rows.
// STATUS values: ROTATED, FAILED, REVERTED, REVERT_FAILED.
func printTier3Table(results []tier3MasterResult) error {
	w := tabwriter.NewWriter(os.Stderr, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "NAME\tSTATUS\tDETAIL"); err != nil {
		return err
	}
	for _, r := range results {
		detail := r.detail
		if detail == "" {
			detail = "-"
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\n", r.name, r.status, detail); err != nil {
			return err
		}
	}
	return w.Flush()
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
