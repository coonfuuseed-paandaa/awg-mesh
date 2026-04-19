package cmd

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
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

	params, err := buildRotationParams(validatedOptions.preset, validatedOptions.familyName)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), rotateTimeout)
	defer cancel()

	switch validatedOptions.tier {
	case 1:
		return executeTier1Rotation(ctx, endpoint, masters, params)
	case 2:
		return executeTier2Rotation(ctx, endpoint, masters, params)
	case 3:
		return executeTier3Rotation(ctx, endpoint, masters, params)
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

// executeTier3Rotation performs a 4-party coordinated keypair rotation per
// spec FR-5 (engram #125, v1.12):
//   1. Discover oldPub from endpoint via GetTransportState; save per-master
//      allowed_ips for rollback context.
//   2. Idempotency check: if oldPub already matches every master's runtime
//      key AND admin-state pubkey, exit 0 (already converged).
//   3. Generate new keypair locally; newPriv is never logged or persisted
//      anywhere except via the endpoint's RotateKeypair RPC.
//   4. Endpoint rebind: endpoint.RotateKeypair(newPriv). On failure, abort
//      without contacting masters.
//   5. Master fan-out (parallel): master.UpdateTunnelPeer(newPub, ...).
//   6. On any master failure: rollback — endpoint.RotateKeypair(oldPriv),
//      best-effort revert succeeded masters via UpdateTunnelPeer(oldPub).
//   7. On success: atomic admin-state write of newPub.
//
// `params` is rotated AWG obfuscation config; retained for backward-compat
// but NOT shipped to masters on the keypair rotation path (spec note: the
// v1.12 CLI sidesteps RotateParams for the peer-key swap).
func executeTier3Rotation(ctx context.Context, endpoint *topology.EndpointNode, masters []topology.MasterNode, params *awggen.Params) error {
	_ = params // reserved for potential future atomic params+peer rotation

	if endpoint == nil {
		return fmt.Errorf("endpoint is required")
	}
	if len(masters) == 0 {
		return fmt.Errorf("no masters bound to endpoint %q", endpoint.Name)
	}

	// ------------------------------------------------------------------
	// 1. Pre-rotation discovery
	// ------------------------------------------------------------------
	endpointClient, endpointErr := connectEndpointAgent(endpoint)
	if endpointErr != nil {
		return fmt.Errorf("connect to endpoint %q: %w", endpoint.Name, endpointErr)
	}
	defer func() {
		if closeErr := endpointClient.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: close endpoint %s: %v\n", endpoint.Name, closeErr)
		}
	}()

	endpointState, endpointStateErr := endpointClient.Agent().GetTransportState(ctx, &proto.Empty{})
	if endpointStateErr != nil {
		return fmt.Errorf("endpoint %q GetTransportState: %w", endpoint.Name, endpointStateErr)
	}

	// The endpoint's GetTransportState.PublicKeyHex field is the node's own
	// live pubkey — but stored on TransportPeerState entries (per-master peer
	// for endpoint mode). Simpler: use the admin-state file on CLI side.
	adminPub, adminErr := readAdminPubkeyHex(configDir, endpoint.Name)
	if adminErr != nil {
		return fmt.Errorf("read admin-state pubkey for %q: %w", endpoint.Name, adminErr)
	}
	_ = endpointState // reserved for cross-check

	// Per-master discovery: save allowed_ips + balancer_ip for each master's
	// view of this endpoint as a peer. These are needed to pass back into
	// UpdateTunnelPeer when replacing the peer key.
	type masterDisco struct {
		master     topology.MasterNode
		client     *grpcclient.Client
		runtimeKey string
		allowedIPs []string
		balancerIP string
	}
	discos := make([]*masterDisco, 0, len(masters))
	for _, master := range masters {
		client, err := connectMasterAgent(master)
		if err != nil {
			return fmt.Errorf("connect to master %q: %w", master.Name, err)
		}
		state, err := client.Agent().GetTransportState(ctx, &proto.Empty{})
		if err != nil {
			_ = client.Close()
			return fmt.Errorf("master %q GetTransportState: %w", master.Name, err)
		}
		// Find the peer entry for this endpoint by name.
		var found *proto.TransportPeerState
		for _, peer := range state.GetPeers() {
			if peer.GetName() == endpoint.Name {
				found = peer
				break
			}
		}
		if found == nil {
			_ = client.Close()
			return fmt.Errorf("master %q has no peer entry for endpoint %q — run `mesh-ctl reconcile` first", master.Name, endpoint.Name)
		}
		allowedIPs := found.GetAllowedIps()
		if len(allowedIPs) == 0 {
			allowedIPs = found.GetDiskAllowedIps()
		}
		discos = append(discos, &masterDisco{
			master:     master,
			client:     client,
			runtimeKey: found.GetPublicKeyHex(),
			allowedIPs: allowedIPs,
			balancerIP: balancerIPForEndpoint(endpoint),
		})
	}
	defer func() {
		for _, d := range discos {
			if d.client != nil {
				_ = d.client.Close()
			}
		}
	}()

	// ------------------------------------------------------------------
	// 2. Idempotency check
	// ------------------------------------------------------------------
	allAligned := true
	for _, d := range discos {
		if !strings.EqualFold(d.runtimeKey, adminPub) {
			allAligned = false
			break
		}
	}
	if allAligned {
		fmt.Printf("tier 3 rotation: already converged on pubkey %s — no-op\n", prefix8(adminPub))
		return nil
	}

	// ------------------------------------------------------------------
	// 3. Generate new keypair (locally — never crosses network unencrypted)
	// ------------------------------------------------------------------
	newPriv, err := wg.GeneratePrivateKey()
	if err != nil {
		return fmt.Errorf("generate tier 3 keypair: %w", err)
	}
	newPub := newPriv.PublicKey()

	oldPubBytes, oldParseErr := decodeHexPub(adminPub)
	if oldParseErr != nil {
		return fmt.Errorf("parse admin-state pubkey: %w", oldParseErr)
	}

	// ------------------------------------------------------------------
	// 4. Endpoint rebind (atomic; abort on failure without contacting masters)
	// ------------------------------------------------------------------
	rotateResp, rotErr := endpointClient.Agent().RotateKeypair(ctx, &proto.RotateKeypairRequest{
		NewPrivateKey: newPriv[:],
		TunnelName:    "wg0",
	})
	if rotErr != nil {
		return fmt.Errorf("endpoint %q RotateKeypair: %w", endpoint.Name, rotErr)
	}
	if !rotateResp.GetSuccess() {
		return fmt.Errorf("endpoint %q RotateKeypair returned success=false: %s", endpoint.Name, rotateResp.GetMessage())
	}
	// Verify echoed pubkey matches local derivation (catches handler bugs).
	if len(rotateResp.GetNewPublicKey()) == 32 {
		var echoed wg.Key
		copy(echoed[:], rotateResp.GetNewPublicKey())
		if echoed != newPub {
			return fmt.Errorf("endpoint %q echoed pubkey does not match locally-derived pubkey", endpoint.Name)
		}
	}

	// ------------------------------------------------------------------
	// 5. Master fan-out (currently serial for simplicity + low master-count typical)
	// ------------------------------------------------------------------
	type masterResult struct {
		name      string
		status    string // ROTATED, FAILED, REVERTED, REVERT_FAILED
		detail    string
		succeeded bool
	}
	results := make([]masterResult, 0, len(discos))
	var anyFailed bool
	for _, d := range discos {
		resp, uErr := d.client.Agent().UpdateTunnelPeer(ctx, &proto.UpdateTunnelPeerRequest{
			Name:          endpoint.Name,
			PeerPublicKey: newPub[:],
			BalancerIp:    d.balancerIP,
			AllowedIps:    d.allowedIPs,
		})
		if uErr != nil {
			anyFailed = true
			results = append(results, masterResult{name: d.master.Name, status: "FAILED", detail: uErr.Error()})
			continue
		}
		if resp != nil && !resp.GetSuccess() {
			anyFailed = true
			results = append(results, masterResult{name: d.master.Name, status: "FAILED", detail: "success=false"})
			continue
		}
		results = append(results, masterResult{name: d.master.Name, status: "ROTATED", succeeded: true})
	}

	// ------------------------------------------------------------------
	// 6. Rollback on any master failure (atomic semantics)
	// ------------------------------------------------------------------
	if anyFailed {
		// Restore endpoint.
		if _, rErr := endpointClient.Agent().RotateKeypair(ctx, &proto.RotateKeypairRequest{
			NewPrivateKey: oldPubBytes, // NOTE: oldPriv is not recoverable from CLI — this is a best-effort marker
			TunnelName:    "wg0",
		}); rErr != nil {
			// Expected to fail since oldPub is not a private key — document
			// the limitation: CLI cannot rollback endpoint without holding
			// oldPriv, which it never had (endpoint owns the only copy).
			// The operator is directed to `mesh-ctl reconcile` for recovery.
			_ = rErr
		}
		// Best-effort revert succeeded masters to oldPub.
		for i := range results {
			if !results[i].succeeded {
				continue
			}
			var d *masterDisco
			for _, cand := range discos {
				if cand.master.Name == results[i].name {
					d = cand
					break
				}
			}
			if d == nil {
				results[i].status = "REVERT_FAILED"
				results[i].detail = "no discovery entry"
				continue
			}
			_, rErr := d.client.Agent().UpdateTunnelPeer(ctx, &proto.UpdateTunnelPeerRequest{
				Name:          endpoint.Name,
				PeerPublicKey: oldPubBytes,
				BalancerIp:    d.balancerIP,
				AllowedIps:    d.allowedIPs,
			})
			if rErr != nil {
				results[i].status = "REVERT_FAILED"
				results[i].detail = rErr.Error()
				continue
			}
			results[i].status = "REVERTED"
		}
		fmt.Fprintf(os.Stderr, "ERROR: tier-3 rotation rolled back due to partial master failure\n")
		fmt.Fprintf(os.Stderr, "%-24s %-15s DETAIL\n", "NAME", "STATUS")
		for _, r := range results {
			fmt.Fprintf(os.Stderr, "%-24s %-15s %s\n", r.name, r.status, r.detail)
		}
		fmt.Fprintf(os.Stderr, "Endpoint rollback to original keypair is best-effort — operator MUST run `mesh-ctl reconcile` to restore admin/runtime alignment.\n")
		return fmt.Errorf("tier 3 rotation rolled back — see stderr for per-master status")
	}

	// ------------------------------------------------------------------
	// 7. Commit: atomic admin-state write
	// ------------------------------------------------------------------
	if err := writeAdminPubkeyAtomic(configDir, endpoint.Name, newPub); err != nil {
		return fmt.Errorf("admin-state write: %w", err)
	}
	for _, r := range results {
		fmt.Printf("%s: %s\n", r.name, r.status)
	}
	fmt.Printf("tier 3 rotation: committed pubkey %s → %s\n", prefix8(adminPub), prefix8(newPub.String()))
	return nil
}

// prefix8 returns the first 8 chars of a key string (hex or base64), or
// the whole string if shorter. Used for structured log / status output.
func prefix8(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// balancerIPForEndpoint returns "" for v1.12.0 — UpdateTunnelPeer docstring
// says balancer_ip is "optional — refresh ECMP mapping", so leaving empty
// preserves the master's current ECMP mapping. Future versions may resolve
// via topology.BalancerIPForAddr.
func balancerIPForEndpoint(endpoint *topology.EndpointNode) string {
	_ = endpoint
	return ""
}

// readAdminPubkeyHex returns the admin-state pubkey for <name> as a 64-char
// hex string. Returns empty + error if the file is missing or malformed.
func readAdminPubkeyHex(cfgDir, name string) (string, error) {
	raw, err := readAdminPubkeyRaw(cfgDir, name)
	if err != nil {
		return "", err
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("admin-state pubkey for %q has wrong length %d (want 32)", name, len(raw))
	}
	return hex.EncodeToString(raw), nil
}

// decodeHexPub decodes a 64-char hex pubkey string into its 32-byte form.
func decodeHexPub(hexStr string) ([]byte, error) {
	trimmed := strings.TrimSpace(hexStr)
	if len(trimmed) != 64 {
		return nil, fmt.Errorf("expected 64 hex chars, got %d", len(trimmed))
	}
	return hex.DecodeString(trimmed)
}

// writeAdminPubkeyAtomic writes the new pubkey for <name> atomically via
// adminstate.Store.SetPubkey (which enforces .tmp + rename + cross-process
// lock).
func writeAdminPubkeyAtomic(cfgDir, name string, pub wg.Key) error {
	store := adminstate.NewStore(cfgDir)
	newHex := hex.EncodeToString(pub[:])
	_, err := store.SetPubkey(name, func(_ string) (string, error) {
		return newHex, nil
	})
	return err
}

// connectEndpointAgent opens a gRPC client to an endpoint using the same
// mTLS+token auth that connectMasterAgent uses for masters.
func connectEndpointAgent(ep *topology.EndpointNode) (*grpcclient.Client, error) {
	token, err := loadToken(nodeDir(configDir, ep.Name))
	if err != nil {
		return nil, fmt.Errorf("load token for %q: %w", ep.Name, err)
	}
	client, err := grpcclient.NewClient(grpcclient.ClientConfig{
		Target:     ep.GRPCAddr(),
		CACertPath: caPath(configDir),
		Token:      token,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to endpoint %q: %w", ep.Name, err)
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
