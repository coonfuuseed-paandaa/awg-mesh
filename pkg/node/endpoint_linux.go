//go:build linux

package node

import (
	"fmt"

	"github.com/amnezia-vpn/amneziawg-go/device"
	"github.com/thebtf/awg-mesh/pkg/wg"
)

const endpointInterfaceName = "wg0"

type endpointPlatformState struct {
	iface *wg.Interface
}

func (e *EndpointRunner) createInterface() error {
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner node is required")
	}

	privateKey, _, err := EnsureKeypair(e.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}

	mtu := CalculateMTU(1420, 80, 1)
	iface, err := wg.NewInterface(
		endpointInterfaceName,
		mtu,
		device.NewLogger(device.LogLevelError, "[endpoint] "),
	)
	if err != nil {
		return fmt.Errorf("create interface %q: %w", endpointInterfaceName, err)
	}

	cfg := wg.Config{
		PrivateKey: &privateKey,
		ListenPort: wg.IntPtr(e.node.config.ListenPort),
	}
	if err := iface.Configure(cfg); err != nil {
		_ = iface.Close()
		return fmt.Errorf("configure interface %q: %w", endpointInterfaceName, err)
	}

	e.platformState.iface = iface
	e.node.logger.Info().
		Str("interface", iface.Name()).
		Int("mtu", mtu).
		Msg("endpoint interface created")

	return nil
}

func (e *EndpointRunner) closeInterface() error {
	if e == nil || e.platformState.iface == nil {
		return nil
	}

	iface := e.platformState.iface
	e.platformState.iface = nil
	return iface.Close()
}

func (e *EndpointRunner) ApplyParams(tunnelName string, cfg wg.Config) error {
	if e == nil || e.node == nil {
		return fmt.Errorf("endpoint runner node is required")
	}
	if e.platformState.iface == nil {
		return fmt.Errorf("endpoint interface is not initialized")
	}

	if err := e.platformState.iface.Configure(cfg); err != nil {
		return fmt.Errorf("configure endpoint interface %q: %w", endpointInterfaceName, err)
	}

	e.node.logger.Info().
		Str("tunnel", tunnelName).
		Str("interface", endpointInterfaceName).
		Msg("applied AWG parameters to endpoint interface")

	return nil
}
