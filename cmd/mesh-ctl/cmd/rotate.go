package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/awggen"
	grpcclient "github.com/coonfuuseed-paandaa/awg-mesh/pkg/grpc"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/rotation"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
)

const (
	defaultRotatePreset = "Balanced"
	rotateAgentPort     = "9090"
	rotateTimeout       = 30 * time.Second
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

func executeTier3Rotation(ctx context.Context, endpoint *topology.EndpointNode, masters []topology.MasterNode, params *awggen.Params) error {
	privateKey, err := wg.GeneratePrivateKey()
	if err != nil {
		return fmt.Errorf("generate tier 3 private key: %w", err)
	}
	publicKey := privateKey.PublicKey()

	rotator := rotation.NewTier3Rotation()
	failures := make([]string, 0)

	for _, master := range masters {
		client, err := connectMasterAgent(master)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: tier 3 rotation failed: %v\n", master.Name, err)
			failures = append(failures, fmt.Sprintf("%s: %v", master.Name, err))
			continue
		}

		executeErr := rotator.Execute(ctx, client.Agent(), endpoint.Name, params, publicKey)
		if executeErr != nil {
			fmt.Fprintf(os.Stderr, "%s: tier 3 rotation failed: %v\n", master.Name, executeErr)
			failures = append(failures, fmt.Sprintf("%s: %v", master.Name, executeErr))
		} else {
			fmt.Printf("%s: tier 3 rotation succeeded\n", master.Name)
		}

		if closeErr := client.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: close %s: %v\n", master.Name, closeErr)
		}
	}

	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("tier 3 rotation failed: %s", strings.Join(failures, "; "))
	}

	return nil
}

func connectMasterAgent(master topology.MasterNode) (*grpcclient.Client, error) {
	token, err := loadToken(nodeDir(configDir, master.Name))
	if err != nil {
		return nil, fmt.Errorf("load token for %q: %w", master.Name, err)
	}

	client, err := grpcclient.NewClient(grpcclient.ClientConfig{
		Target:     net.JoinHostPort(master.Host, rotateAgentPort),
		Insecure: true,
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
