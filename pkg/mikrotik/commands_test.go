package mikrotik

import (
	"reflect"
	"strings"
	"testing"
)

func TestGenerateContainerCommands(t *testing.T) {
	t.Parallel()

	cfg := ContainerConfig{
		Name:      "awg node",
		Image:     "ghcr.io/thebtf/awg-mesh:latest",
		Interface: "veth1",
		EnvVars: map[string]string{
			"Z_VAR": "z",
			"A_VAR": "value with space",
		},
	}

	commands := GenerateContainerCommands(cfg)
	if len(commands) != 3 {
		t.Fatalf("expected 3 commands, got %d: %#v", len(commands), commands)
	}

	if !strings.Contains(commands[0], "key=A_VAR") {
		t.Fatalf("expected sorted env var command first, got %q", commands[0])
	}
	if !strings.Contains(commands[1], "key=Z_VAR") {
		t.Fatalf("expected sorted env var command second, got %q", commands[1])
	}

	last := commands[len(commands)-1]
	checks := []string{
		"/container/add",
		"interface=veth1",
		"image=ghcr.io/thebtf/awg-mesh:latest",
		"hostname=\"awg node\"",
		"envlist=\"awg node-envs\"",
		"name=\"awg node\"",
	}
	for _, check := range checks {
		if !strings.Contains(last, check) {
			t.Fatalf("expected container command to contain %q, got %q", check, last)
		}
	}
}

func TestGenerateVethCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		gateway         string
		wantAddressPart string
		wantGatewayPart string
	}{
		{name: "empty gateway uses default", gateway: "", wantAddressPart: "address=192.168.100.1/24", wantGatewayPart: "gateway=192.168.100.2"},
		{name: "prefix gateway", gateway: "10.0.0.1/24", wantAddressPart: "address=10.0.0.1/24", wantGatewayPart: "gateway=10.0.0.2"},
		{name: "ipv4 address gateway", gateway: "10.0.0.3", wantAddressPart: "address=10.0.0.2/24", wantGatewayPart: "gateway=10.0.0.3"},
		{name: "invalid gateway preserved", gateway: "bad-value", wantAddressPart: "address=bad-value/24", wantGatewayPart: "gateway=bad-value"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			commands := GenerateVethCommands("veth-test", tt.gateway)
			if len(commands) != 1 {
				t.Fatalf("expected one command, got %d", len(commands))
			}

			if !strings.Contains(commands[0], tt.wantAddressPart) {
				t.Fatalf("expected %q in command %q", tt.wantAddressPart, commands[0])
			}
			if !strings.Contains(commands[0], tt.wantGatewayPart) {
				t.Fatalf("expected %q in command %q", tt.wantGatewayPart, commands[0])
			}
		})
	}
}

func TestGenerateRouteAndFirewallCommands(t *testing.T) {
	t.Parallel()

	routeCommands := GenerateRouteCommands("10.0.0.0/16", "10.0.0.1")
	if !reflect.DeepEqual(routeCommands, []string{"/ip/route add dst-address=10.0.0.0/16 gateway=10.0.0.1"}) {
		t.Fatalf("unexpected route commands: %#v", routeCommands)
	}

	firewallCommands := GenerateFirewallCommands("veth-main")
	if len(firewallCommands) != 2 {
		t.Fatalf("expected 2 firewall commands, got %d", len(firewallCommands))
	}
	if !strings.Contains(firewallCommands[0], "in-interface=veth-main") {
		t.Fatalf("unexpected first firewall command: %q", firewallCommands[0])
	}
	if !strings.Contains(firewallCommands[1], "out-interface=veth-main") {
		t.Fatalf("unexpected second firewall command: %q", firewallCommands[1])
	}
}

func TestEscapeRouterOSToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "plain", input: "value", want: "value"},
		{name: "empty", input: "   ", want: "\"\""},
		{name: "space", input: "value with space", want: "\"value with space\""},
		{name: "quote", input: "say \"hi\"", want: "\"say \\\"hi\\\"\""},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := escapeRouterOSToken(tt.input)
			if got != tt.want {
				t.Fatalf("escapeRouterOSToken(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
