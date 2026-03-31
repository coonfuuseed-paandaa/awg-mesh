package rotation

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/awggen"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
)

// Tier1Rotation executes zero-downtime per-tunnel rotation of Jc/Jmin/Jmax and I1-I5 only.
// This tier can be applied to a single master without coordination across masters.
type Tier1Rotation struct{}

func NewTier1Rotation() *Tier1Rotation {
	return &Tier1Rotation{}
}

// Execute sends a Tier 1 rotate request to a single master agent.
// Only Jc, Jmin, Jmax, and I1-I5 are included in new_params; S1-S4 and H1-H4 are zeroed.
func (r *Tier1Rotation) Execute(ctx context.Context, client proto.AwgAgentClient, tunnelName string, params *awggen.Params) error {
	if err := validateTier1Inputs(ctx, client, tunnelName, params); err != nil {
		return err
	}

	req := &proto.RotateParamsRequest{
		TunnelName: tunnelName,
		Tier:       1,
		NewParams: &proto.AwgParams{
			Jc:   int32(params.Jc),
			Jmin: int32(params.Jmin),
			Jmax: int32(params.Jmax),
			I1:   params.I1,
			I2:   params.I2,
			I3:   params.I3,
			I4:   params.I4,
			I5:   params.I5,
		},
	}

	resp, err := client.RotateParams(ctx, req)
	if err != nil {
		return fmt.Errorf("tier 1 rotate %q: %w", tunnelName, err)
	}
	if resp == nil {
		return fmt.Errorf("tier 1 rotate %q: empty response", tunnelName)
	}
	if !resp.GetSuccess() {
		return fmt.Errorf("tier 1 rotate %q failed: %s", tunnelName, resp.GetMessage())
	}

	log.Info().Str("tunnel", tunnelName).Msg("tier 1 rotation applied")
	return nil
}

func validateTier1Inputs(ctx context.Context, client proto.AwgAgentClient, tunnelName string, params *awggen.Params) error {
	if ctx == nil {
		return fmt.Errorf("tier 1 execute: context is required")
	}
	if client == nil {
		return fmt.Errorf("tier 1 execute: client is required")
	}
	if strings.TrimSpace(tunnelName) == "" {
		return fmt.Errorf("tier 1 execute: tunnel name is required")
	}
	if params == nil {
		return fmt.Errorf("tier 1 execute: params are required")
	}

	return nil
}
