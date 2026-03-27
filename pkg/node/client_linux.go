//go:build linux

package node

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/amnezia-vpn/amneziawg-go/device"
	"github.com/thebtf/awg-mesh/pkg/topology"
	"github.com/thebtf/awg-mesh/pkg/wg"
)

const clientInterfacePrefix = "wg-"

type clientPlatformState struct {
	ifaces []*wg.Interface
}

func (c *ClientRunner) createInterfaces(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if c == nil || c.node == nil {
		return fmt.Errorf("client runner node is required")
	}
	if c.node.topology == nil || len(c.node.topology.Masters) == 0 {
		c.node.logger.Warn().Msg("client topology has no masters; no AWG interfaces created")
		return nil
	}

	privateKey, _, err := EnsureKeypair(c.node.config.ConfigDir)
	if err != nil {
		return fmt.Errorf("ensure keypair: %w", err)
	}

	mtu := CalculateMTU(1420, 80, 1)
	createdInterfaces := make([]*wg.Interface, 0, len(c.node.topology.Masters))
	for _, master := range c.node.topology.Masters {
		if err := ctx.Err(); err != nil {
			cleanupErr := closeClientInterfaceSlice(createdInterfaces)
			if cleanupErr != nil {
				return fmt.Errorf("context canceled while creating client interfaces: %w; cleanup error: %v", err, cleanupErr)
			}
			return err
		}

		iface, err := c.createMasterInterface(privateKey, master, mtu)
		if err != nil {
			cleanupErr := closeClientInterfaceSlice(createdInterfaces)
			if cleanupErr != nil {
				return fmt.Errorf("create interface for master %q: %w; cleanup error: %v", master.Name, err, cleanupErr)
			}
			return fmt.Errorf("create interface for master %q: %w", master.Name, err)
		}

		createdInterfaces = append(createdInterfaces, iface)
	}

	c.platformState.ifaces = append([]*wg.Interface(nil), createdInterfaces...)
	return nil
}

func (c *ClientRunner) createMasterInterface(privateKey wg.Key, master topology.MasterNode, mtu int) (*wg.Interface, error) {
	interfaceName, err := buildClientInterfaceName(master.Name)
	if err != nil {
		return nil, err
	}

	iface, err := wg.NewInterface(
		interfaceName,
		mtu,
		device.NewLogger(device.LogLevelError, "[client] "),
	)
	if err != nil {
		return nil, fmt.Errorf("create interface %q: %w", interfaceName, err)
	}

	endpointAddr, err := resolvePeerEndpoint(master.Host, master.ListenPort)
	if err != nil {
		_ = iface.Close()
		return nil, fmt.Errorf("resolve master endpoint for %q: %w", master.Name, err)
	}

	c.node.logger.Warn().
		Str("master", master.Name).
		Msg("master public key unavailable in topology; using zero-key peer placeholder")

	cfg := wg.Config{
		PrivateKey: &privateKey,
		Peers: []wg.PeerConfig{
			{
				PublicKey: wg.Key{},
				Endpoint:  endpointAddr,
			},
		},
	}
	if err := iface.Configure(cfg); err != nil {
		_ = iface.Close()
		return nil, fmt.Errorf("configure interface %q: %w", interfaceName, err)
	}

	c.node.logger.Info().
		Str("interface", iface.Name()).
		Str("master", master.Name).
		Str("endpoint", endpointAddr.String()).
		Int("mtu", mtu).
		Msg("client interface created")

	return iface, nil
}

func (c *ClientRunner) closeInterfaces() error {
	if c == nil {
		return nil
	}

	closeErr := closeClientInterfaceSlice(c.platformState.ifaces)
	c.platformState.ifaces = nil
	return closeErr
}

func closeClientInterfaceSlice(ifaces []*wg.Interface) error {
	closeErrors := make([]string, 0, len(ifaces))
	for _, iface := range ifaces {
		if iface == nil {
			continue
		}
		if err := iface.Close(); err != nil {
			closeErrors = append(closeErrors, err.Error())
		}
	}

	if len(closeErrors) == 0 {
		return nil
	}

	return fmt.Errorf("close client interfaces: %s", strings.Join(closeErrors, "; "))
}

func buildClientInterfaceName(masterName string) (string, error) {
	trimmedMasterName := strings.TrimSpace(masterName)
	if trimmedMasterName == "" {
		return "", fmt.Errorf("master name is required")
	}

	return clientInterfacePrefix + trimmedMasterName, nil
}

func resolvePeerEndpoint(host string, port int) (*net.UDPAddr, error) {
	trimmedHost := strings.TrimSpace(host)
	if trimmedHost == "" {
		return nil, fmt.Errorf("host is required")
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port must be between 1 and 65535, got %d", port)
	}

	return net.ResolveUDPAddr("udp", net.JoinHostPort(trimmedHost, strconv.Itoa(port)))
}
