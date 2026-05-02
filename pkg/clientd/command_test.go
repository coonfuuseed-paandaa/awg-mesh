package clientd

import (
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

func TestParseCommandConfigRequiredFlagsAndProtocol(t *testing.T) {
	_, err := ParseCommandConfig(nil, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "--control-plane") || !strings.Contains(err.Error(), "--cert") {
		t.Fatalf("expected missing required flags error, got %v", err)
	}

	args := validCommandArgs(t)
	args[len(args)-1] = "bad-protocol"
	_, err = ParseCommandConfig(args, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "invalid --protocol") {
		t.Fatalf("expected invalid protocol error, got %v", err)
	}

	args[len(args)-1] = string(wg.ProtocolVanilla)
	cfg, err := ParseCommandConfig(args, &strings.Builder{})
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if cfg.Protocol != wg.ProtocolVanilla || cfg.Name != "client-a" {
		t.Fatalf("unexpected parsed config: %#v", cfg)
	}
}

func TestValidateCommandConfigInsecureControlPlaneGate(t *testing.T) {
	for _, target := range []string{"localhost:51820", "127.0.0.1:51820", "127.42.0.1:51820", "[::1]:51820"} {
		t.Run("accept_loopback_"+target, func(t *testing.T) {
			cfg := validCommandConfig(t)
			cfg.ControlPlane = target
			if _, err := ValidateCommandConfig(cfg); err != nil {
				t.Fatalf("loopback target rejected: %v", err)
			}
		})
	}

	cfg := validCommandConfig(t)
	cfg.ControlPlane = "192.0.2.10:51820"
	if _, err := ValidateCommandConfig(cfg); err == nil || !strings.Contains(err.Error(), "--allow-insecure-control-plane") {
		t.Fatalf("expected non-loopback rejection, got %v", err)
	}

	cfg.AllowInsecureControlPlane = true
	if _, err := ValidateCommandConfig(cfg); err != nil {
		t.Fatalf("override should allow non-loopback target: %v", err)
	}
}

func TestValidateCommandConfigRejectsInvalidInterfaceName(t *testing.T) {
	cfg := validCommandConfig(t)
	cfg.InterfaceName = "../bad"
	if _, err := ValidateCommandConfig(cfg); err == nil || !strings.Contains(err.Error(), "invalid --iface") {
		t.Fatalf("expected invalid interface name rejection, got %v", err)
	}
}

func validCommandArgs(t *testing.T) []string {
	t.Helper()
	return []string{
		"--control-plane", "127.0.0.1:51820",
		"--name", "client-a",
		"--overlay-ip", "10.10.0.10",
		"--region", "eu-test",
		"--cert", "node.pem",
		"--state-dir", t.TempDir(),
		"--iface", "awg-test0",
		"--protocol", string(wg.ProtocolAmneziaWG),
	}
}

func validCommandConfig(t *testing.T) CommandConfig {
	t.Helper()
	cfg, err := ParseCommandConfig(validCommandArgs(t), &strings.Builder{})
	if err != nil {
		t.Fatalf("valid command args rejected: %v", err)
	}
	return cfg
}
