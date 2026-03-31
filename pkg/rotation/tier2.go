package rotation

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/awggen"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
)

// Tier2Rotation executes coordinated rotation of S1-S4 and H1-H4 across all masters.
// A brief mismatch window is expected during simultaneous application.
type Tier2Rotation struct{}

func NewTier2Rotation() *Tier2Rotation {
	return &Tier2Rotation{}
}

// Execute performs a preflight health check on all clients, then simultaneously
// sends a Tier 2 rotate request to all masters.
func (r *Tier2Rotation) Execute(ctx context.Context, clients map[string]proto.AwgAgentClient, tunnelName string, params *awggen.Params) error {
	if err := validateTier2Inputs(ctx, clients, tunnelName, params); err != nil {
		return err
	}
	if err := r.preflight(ctx, clients); err != nil {
		return err
	}

	results := make(chan rotateTier2Result, len(clients))
	var wg sync.WaitGroup

	for masterName, client := range clients {
		wg.Add(1)
		go func(name string, c proto.AwgAgentClient) {
			defer wg.Done()
			req := buildTier2RotateRequest(tunnelName, params)
			resp, err := c.RotateParams(ctx, req)
			if err != nil {
				results <- rotateTier2Result{master: name, err: fmt.Errorf("rpc error: %w", err)}
				return
			}
			if resp == nil {
				results <- rotateTier2Result{master: name, err: fmt.Errorf("empty response")}
				return
			}
			if !resp.GetSuccess() {
				results <- rotateTier2Result{master: name, err: fmt.Errorf("rpc returned failure: %s", resp.GetMessage())}
				return
			}
			results <- rotateTier2Result{master: name, err: nil}
		}(masterName, client)
	}

	wg.Wait()
	close(results)

	var failures []string
	for res := range results {
		if res.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", res.master, res.err))
		}
	}

	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("tier 2 rotation partially failed: %s", strings.Join(failures, "; "))
	}

	log.Info().Str("tunnel", tunnelName).Int("masters", len(clients)).Msg("tier 2 rotation applied")
	return nil
}

func (r *Tier2Rotation) preflight(ctx context.Context, clients map[string]proto.AwgAgentClient) error {
	unhealthy := make([]string, 0)
	for name, client := range clients {
		if client == nil {
			unhealthy = append(unhealthy, fmt.Sprintf("%s: client is nil", name))
			continue
		}

		resp, err := client.GetHealth(ctx, &proto.Empty{})
		if err != nil {
			unhealthy = append(unhealthy, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if resp == nil || !resp.GetHealthy() {
			unhealthy = append(unhealthy, fmt.Sprintf("%s: unhealthy", name))
		}
	}
	if len(unhealthy) > 0 {
		sort.Strings(unhealthy)
		return fmt.Errorf("tier 2 preflight failed, unhealthy masters: %s", strings.Join(unhealthy, ", "))
	}
	return nil
}

type rotateTier2Result struct {
	master string
	err    error
}

func buildTier2RotateRequest(tunnelName string, params *awggen.Params) *proto.RotateParamsRequest {
	return &proto.RotateParamsRequest{
		TunnelName: tunnelName,
		Tier:       2,
		NewParams: &proto.AwgParams{
			S1: int32(params.S1),
			S2: int32(params.S2),
			S3: int32(params.S3),
			S4: int32(params.S4),
			H1: int32(params.H1),
			H2: int32(params.H2),
			H3: int32(params.H3),
			H4: int32(params.H4),
		},
	}
}

func validateTier2Inputs(ctx context.Context, clients map[string]proto.AwgAgentClient, tunnelName string, params *awggen.Params) error {
	if ctx == nil {
		return fmt.Errorf("tier 2 execute: context is required")
	}
	if strings.TrimSpace(tunnelName) == "" {
		return fmt.Errorf("tier 2 execute: tunnel name is required")
	}
	if params == nil {
		return fmt.Errorf("tier 2 execute: params are required")
	}
	if len(clients) == 0 {
		return fmt.Errorf("tier 2 execute: at least one master client is required")
	}

	return nil
}
