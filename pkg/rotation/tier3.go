package rotation

import (
	"context"
	"fmt"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/awggen"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
	proto "github.com/coonfuuseed-paandaa/awg-mesh/v2/proto"
	"github.com/rs/zerolog/log"
)

// Tier3Rotation executes keypair rotation with a brief (~2s) reconnect expected.
type Tier3Rotation struct{}

func NewTier3Rotation() *Tier3Rotation {
	return &Tier3Rotation{}
}

// Execute sends a Tier 3 rotate request including a new keypair public key.
// A brief (~2s) reconnect is expected after this operation.
func (r *Tier3Rotation) Execute(ctx context.Context, client proto.AwgAgentClient, tunnelName string, params *awggen.Params, newPublicKey wg.Key) error {
	if err := validateTier3Inputs(ctx, client, tunnelName, params, newPublicKey); err != nil {
		return err
	}

	req := &proto.RotateParamsRequest{
		TunnelName:   tunnelName,
		Tier:         3,
		NewParams:    params.ToProto(),
		NewPublicKey: append([]byte(nil), newPublicKey[:]...),
	}

	resp, err := client.RotateParams(ctx, req)
	if err != nil {
		return fmt.Errorf("tier 3 rotate %q: %w", tunnelName, err)
	}
	if resp == nil {
		return fmt.Errorf("tier 3 rotate %q: empty response", tunnelName)
	}
	if !resp.GetSuccess() {
		return fmt.Errorf("tier 3 rotate %q failed: %s", tunnelName, resp.GetMessage())
	}

	log.Info().Str("tunnel", tunnelName).Msg("tier 3 rotation applied, brief reconnect expected (~2s)")
	return nil
}

func validateTier3Inputs(ctx context.Context, client proto.AwgAgentClient, tunnelName string, params *awggen.Params, newPublicKey wg.Key) error {
	if ctx == nil {
		return fmt.Errorf("tier 3 execute: context is required")
	}
	if client == nil {
		return fmt.Errorf("tier 3 execute: client is required")
	}
	if strings.TrimSpace(tunnelName) == "" {
		return fmt.Errorf("tier 3 execute: tunnel name is required")
	}
	if params == nil {
		return fmt.Errorf("tier 3 execute: params are required")
	}
	if newPublicKey.IsZero() {
		return fmt.Errorf("tier 3 execute: new public key must not be zero")
	}

	return nil
}
