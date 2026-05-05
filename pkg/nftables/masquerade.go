package nftables

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/routing"
)

const (
	defaultTableName       = "awg_mesh"
	defaultPostroutingName = "nat_postrouting"
)

// NATFirewall is the minimal kernel firewall surface needed by egress mode.
type NATFirewall interface {
	SetupNAT(iface string) error
}

// MasqueradeConfig describes the egress boundary NAT rule.
type MasqueradeConfig struct {
	InternetInterface string
}

// MasqueradePlan is the immutable intended NAT boundary state.
type MasqueradePlan struct {
	InternetInterface string
	Table             string
	Chain             string
	Operation         string
}

// MasqueradeInstaller applies the egress boundary MASQUERADE rule.
type MasqueradeInstaller struct {
	firewall NATFirewall
}

// NewKernelMasqueradeInstaller builds an installer backed by the host nftables API.
func NewKernelMasqueradeInstaller() (*MasqueradeInstaller, error) {
	firewall, err := routing.NewNftablesFirewall()
	if err != nil {
		return nil, err
	}
	return NewMasqueradeInstaller(firewall)
}

// NewMasqueradeInstaller builds an installer around an explicit firewall dependency.
func NewMasqueradeInstaller(firewall NATFirewall) (*MasqueradeInstaller, error) {
	if firewall == nil {
		return nil, errors.New("nftables firewall is required")
	}
	return &MasqueradeInstaller{firewall: firewall}, nil
}

// Plan validates config and returns the exact egress NAT boundary shape.
func Plan(cfg MasqueradeConfig) (MasqueradePlan, error) {
	iface, err := normalizeInternetInterface(cfg.InternetInterface)
	if err != nil {
		return MasqueradePlan{}, err
	}
	return MasqueradePlan{
		InternetInterface: iface,
		Table:             defaultTableName,
		Chain:             defaultPostroutingName,
		Operation:         "oifname " + iface + " masquerade",
	}, nil
}

// Apply installs the egress MASQUERADE rule. Idempotency is provided by SetupNAT.
func (i *MasqueradeInstaller) Apply(ctx context.Context, cfg MasqueradeConfig) (MasqueradePlan, error) {
	if i == nil || i.firewall == nil {
		return MasqueradePlan{}, errors.New("masquerade installer is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return MasqueradePlan{}, err
	}
	plan, err := Plan(cfg)
	if err != nil {
		return MasqueradePlan{}, err
	}
	if err := i.firewall.SetupNAT(plan.InternetInterface); err != nil {
		return MasqueradePlan{}, fmt.Errorf("apply egress masquerade on %s: %w", plan.InternetInterface, err)
	}
	if err := ctx.Err(); err != nil {
		return MasqueradePlan{}, err
	}
	return plan, nil
}

func normalizeInternetInterface(name string) (string, error) {
	iface := strings.TrimSpace(name)
	if iface == "" {
		return "", errors.New("internet interface is required")
	}
	if len(iface) > 15 {
		return "", fmt.Errorf("internet interface %q exceeds Linux IFNAMSIZ limit", iface)
	}
	if strings.ContainsAny(iface, " /\t\r\n") {
		return "", fmt.Errorf("internet interface %q contains invalid characters", iface)
	}
	if isMeshInterfaceName(iface) {
		return "", fmt.Errorf("internet interface %q looks like a mesh interface", iface)
	}
	return iface, nil
}

func isMeshInterfaceName(name string) bool {
	lower := strings.ToLower(name)
	return lower == "wg" ||
		strings.HasPrefix(lower, "wg-") ||
		strings.HasPrefix(lower, "wg_") ||
		strings.HasPrefix(lower, "awg") ||
		strings.HasPrefix(lower, "tun")
}
