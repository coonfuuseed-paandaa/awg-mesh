package mikrotik

import (
	"strings"
	"testing"
)

func TestGenerateContainerCommands(t *testing.T) {
	t.Parallel()

	cfg := ContainerConfig{
		Name:      "AWG_MESH_HOME",
		Image:     "ghcr.io/coonfuuseed-paandaa/awg-mesh-client:latest",
		Interface: "AWG_MESH_HOME",
		RootDir:   "/docker/awg-mesh-client-home",
		MountName: "AWG_MESH_HOME_CONFIG",
		MountSrc:  "/docker/etc/awg-mesh-client-home-config",
		DNS:       []string{"1.1.1.1", "8.8.8.8"},
		Command:   "--mode clientd --name mikrotik-home",
		EnvVars: map[string]string{
			"MESH_MODE": "clientd",
			"MESH_NAME": "mikrotik-home",
		},
	}

	commands, err := GenerateContainerCommands(cfg)
	if err != nil {
		t.Fatalf("GenerateContainerCommands: %v", err)
	}

	// Should have: mount + env1 + env2 + container/add = 4 commands
	if len(commands) < 4 {
		t.Fatalf("expected at least 4 commands, got %d: %#v", len(commands), commands)
	}

	joined := strings.Join(commands, "\n")

	mustContain := []string{
		"/container/mounts/add list=AWG_MESH_HOME_CONFIG",
		"src=/docker/etc/awg-mesh-client-home-config",
		"dst=/config",
		"list=AWG_MESH_HOME_ENVS",
		"key=MESH_MODE",
		"key=MESH_NAME",
		"/container/add interface=AWG_MESH_HOME",
		"root-dir=/docker/awg-mesh-client-home",
		"remote-image=ghcr.io/coonfuuseed-paandaa/awg-mesh-client:latest",
		"mountlists=AWG_MESH_HOME_CONFIG",
		"dns=1.1.1.1,8.8.8.8",
		`cmd="--mode clientd --name mikrotik-home"`,
		"logging=yes",
		"start-on-boot=yes",
	}

	for _, check := range mustContain {
		if !strings.Contains(joined, check) {
			t.Errorf("expected commands to contain %q, got:\n%s", check, joined)
		}
	}

	// RouterOS 7.21+ requires list= not name= for envs
	for _, cmd := range commands {
		if strings.Contains(cmd, "/container/envs/add") {
			if !strings.Contains(cmd, "list=") {
				t.Errorf("env command missing list= parameter: %q", cmd)
			}
		}
	}
}

func TestGenerateContainerCommandsSelectsRouterOSDialect(t *testing.T) {
	t.Parallel()

	base := ContainerConfig{
		Name:      "AWG_MESH_HOME",
		Image:     "ghcr.io/coonfuuseed-paandaa/awg-mesh-client:latest",
		Interface: "AWG_MESH_HOME",
		RootDir:   "/docker/awg-mesh-client-home",
		MountName: "AWG_MESH_HOME_CONFIG",
		MountSrc:  "/docker/etc/awg-mesh-client-home-config",
		EnvVars:   map[string]string{"MESH_MODE": "client"},
	}

	tests := []struct {
		name      string
		targetROS string
		want      []string
		notWant   []string
	}{
		{
			name:      "legacy 7.16",
			targetROS: "7.16.2",
			want: []string{
				"/container/mounts/add name=AWG_MESH_HOME_CONFIG",
				"/container/envs/add name=AWG_MESH_HOME_ENVS",
				"image=ghcr.io/coonfuuseed-paandaa/awg-mesh-client:latest",
				"mounts=AWG_MESH_HOME_CONFIG",
			},
			notWant: []string{"mountlists=", "remote-image="},
		},
		{
			name:      "transitional 7.20",
			targetROS: "7.20.8",
			want: []string{
				"/container/mounts/add name=AWG_MESH_HOME_CONFIG",
				"/container/envs/add name=AWG_MESH_HOME_ENVS",
				"remote-image=ghcr.io/coonfuuseed-paandaa/awg-mesh-client:latest",
				"mounts=AWG_MESH_HOME_CONFIG",
			},
			notWant: []string{"mountlists="},
		},
		{
			name:      "early 7.21 remains transitional",
			targetROS: "7.21.3",
			want: []string{
				"/container/mounts/add name=AWG_MESH_HOME_CONFIG",
				"/container/envs/add name=AWG_MESH_HOME_ENVS",
				"remote-image=ghcr.io/coonfuuseed-paandaa/awg-mesh-client:latest",
				"mounts=AWG_MESH_HOME_CONFIG",
			},
			notWant: []string{"mountlists=", "/container/envs/add list="},
		},
		{
			name:      "canonical 7.21",
			targetROS: "7.21.4",
			want: []string{
				"/container/mounts/add list=AWG_MESH_HOME_CONFIG",
				"/container/envs/add list=AWG_MESH_HOME_ENVS",
				"remote-image=ghcr.io/coonfuuseed-paandaa/awg-mesh-client:latest",
				"mountlists=AWG_MESH_HOME_CONFIG",
			},
			notWant: []string{"mounts=AWG_MESH_HOME_CONFIG", "/container/envs/add name="},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := base
			cfg.TargetROSVersion = tt.targetROS
			commands, err := GenerateContainerCommandsForTarget(cfg)
			if err != nil {
				t.Fatalf("GenerateContainerCommandsForTarget: %v", err)
			}
			joined := strings.Join(commands, "\n")
			for _, want := range tt.want {
				if !strings.Contains(joined, want) {
					t.Errorf("expected %q in commands:\n%s", want, joined)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(joined, notWant) {
					t.Errorf("did not expect %q in commands:\n%s", notWant, joined)
				}
			}
		})
	}
}

func TestGenerateContainerCommandsDoesNotReferenceMissingMount(t *testing.T) {
	t.Parallel()

	commands, err := GenerateContainerCommandsForTarget(ContainerConfig{
		Name:             "AWG_MESH_HOME",
		Image:            "ghcr.io/coonfuuseed-paandaa/awg-mesh-client:latest",
		Interface:        "AWG_MESH_HOME",
		MountName:        "AWG_MESH_HOME_CONFIG",
		TargetROSVersion: "7.21.4",
	})
	if err != nil {
		t.Fatalf("GenerateContainerCommandsForTarget: %v", err)
	}
	joined := strings.Join(commands, "\n")
	if strings.Contains(joined, "/container/mounts/add") || strings.Contains(joined, "mountlists=AWG_MESH_HOME_CONFIG") {
		t.Fatalf("commands referenced an undefined mount:\n%s", joined)
	}
}

func TestGenerateContainerCommandsRejectsUnsupportedRouterOS(t *testing.T) {
	t.Parallel()

	for _, targetROS := range []string{"7.4.0", "7.22.1", "6.49.10", "7.21.bad"} {
		_, err := GenerateContainerCommandsForTarget(ContainerConfig{
			Name:             "AWG_MESH_HOME",
			Image:            "image",
			Interface:        "veth1",
			TargetROSVersion: targetROS,
		})
		if err == nil {
			t.Fatalf("expected target RouterOS %s to be rejected", targetROS)
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
		{name: "empty gateway uses CGN default", gateway: "", wantAddressPart: "address=100.127.0.2/24", wantGatewayPart: "gateway=100.127.0.1"},
		{name: "prefix gateway", gateway: "10.0.0.1/24", wantAddressPart: "address=10.0.0.2/24", wantGatewayPart: "gateway=10.0.0.1"},
		{name: "ipv4 address gateway", gateway: "10.0.0.3", wantAddressPart: "address=10.0.0.2/24", wantGatewayPart: "gateway=10.0.0.3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			commands := GenerateVethCommands("AWG_MESH_TEST", tt.gateway)
			if len(commands) != 1 {
				t.Fatalf("expected one command, got %d", len(commands))
			}
			if !strings.Contains(commands[0], tt.wantAddressPart) {
				t.Errorf("expected %q in command %q", tt.wantAddressPart, commands[0])
			}
			if !strings.Contains(commands[0], tt.wantGatewayPart) {
				t.Errorf("expected %q in command %q", tt.wantGatewayPart, commands[0])
			}
		})
	}
}

func TestGenerateRouteCommands(t *testing.T) {
	t.Parallel()

	cmds := GenerateRouteCommands("10.0.0.0/16", "10.0.0.1")
	if len(cmds) != 1 {
		t.Fatalf("expected 1 route command, got %d", len(cmds))
	}
	mustContain := []string{
		"/ip/route add dst-address=10.0.0.0/16",
		"comment=\"awg-mesh: overlay network\"",
	}
	for _, check := range mustContain {
		if !strings.Contains(cmds[0], check) {
			t.Errorf("expected route command to contain %q, got %q", check, cmds[0])
		}
	}
}

func TestGenerateFirewallCommands(t *testing.T) {
	t.Parallel()

	cmds := GenerateFirewallCommands("BR_AWG_MESH")
	if len(cmds) != 1 {
		t.Fatalf("expected 1 firewall script block, got %d", len(cmds))
	}

	// The generated block is a RouterOS scripting if/else construct anchored on
	// action=fasttrack-connection, which is universal across standard installs.
	mustContain := []string{
		":local fastTrackId [/ip/firewall/filter find where action=fasttrack-connection chain=forward]",
		":if ([:len $fastTrackId] > 0) do={",
		"connection-state=established,related",
		"in-interface=BR_AWG_MESH",
		"place-before=$fastTrackId",
		`"awg-mesh: established return traffic"`,
		`"awg-mesh: container outbound"`,
	}
	joined := strings.Join(cmds, "\n")
	for _, check := range mustContain {
		if !strings.Contains(joined, check) {
			t.Errorf("expected firewall script to contain %q, got:\n%s", check, joined)
		}
	}
}

func TestGenerateFirewallCommandsFallback(t *testing.T) {
	t.Parallel()

	// GenerateFirewallCommands always emits a RouterOS if/else block; the else
	// branch handles stripped-install routers where action=fasttrack-connection
	// does not exist. Both branches are always present in the generated output;
	// RouterOS evaluates which path to take at /import time.
	cmds := GenerateFirewallCommands("BR_AWG_MESH")
	joined := strings.Join(cmds, "\n")

	// Else branch: warning comment + no place-before= on the fallback rules.
	if !strings.Contains(joined, "# WARNING: no fasttrack-connection rule found, appended to chain end") {
		t.Errorf("firewall script missing fallback warning comment, got:\n%s", joined)
	}
	if !strings.Contains(joined, "} else={") {
		t.Errorf("firewall script missing else branch, got:\n%s", joined)
	}

	// The else branch must include the rules WITHOUT place-before=.
	// Verify the else block contains the firewall adds sans place-before.
	elseIdx := strings.Index(joined, "} else={")
	if elseIdx < 0 {
		t.Fatalf("else block not found in:\n%s", joined)
	}
	elseBlock := joined[elseIdx:]
	if strings.Contains(elseBlock, "place-before=") {
		t.Errorf("else branch must NOT contain place-before=, got:\n%s", elseBlock)
	}
	if !strings.Contains(elseBlock, "action=accept connection-state=established,related") {
		t.Errorf("else branch must contain established rule, got:\n%s", elseBlock)
	}
}

func TestGenerateBridgeCommands(t *testing.T) {
	t.Parallel()

	cmds := GenerateBridgeCommands("BR_AWG_MESH", "AWG_MESH_HOME", "100.127.0.1")
	if len(cmds) != 3 {
		t.Fatalf("expected 3 bridge commands, got %d", len(cmds))
	}
	joined := strings.Join(cmds, "\n")

	mustContain := []string{
		"/interface/bridge/add name=BR_AWG_MESH",
		"/ip/address/add address=100.127.0.1/24 interface=BR_AWG_MESH",
		"/interface/bridge/port add bridge=BR_AWG_MESH interface=AWG_MESH_HOME",
	}
	for _, check := range mustContain {
		if !strings.Contains(joined, check) {
			t.Errorf("expected bridge commands to contain %q, got:\n%s", check, joined)
		}
	}
}

func TestGenerateNATCommands(t *testing.T) {
	t.Parallel()

	cmds := GenerateNATCommands("100.127.0.1", 9090)
	if len(cmds) != 2 {
		t.Fatalf("expected 2 NAT commands (masquerade + dstnat), got %d", len(cmds))
	}
	if !strings.Contains(cmds[0], "srcnat action=masquerade") {
		t.Errorf("first NAT command should be masquerade, got %q", cmds[0])
	}
	if !strings.Contains(cmds[1], "dst-port=9090") {
		t.Errorf("second NAT command should have dst-port=9090, got %q", cmds[1])
	}

	// Without gRPC port — only masquerade
	cmds2 := GenerateNATCommands("100.127.0.1", 0)
	if len(cmds2) != 1 {
		t.Fatalf("expected 1 NAT command without gRPC port, got %d", len(cmds2))
	}
}

func TestDeriveContainerNameAndHelpers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input     string
		wantName  string
		wantEnv   string
		wantMount string
	}{
		{"mikrotik-home", "AWG_MESH_MIKROTIK_HOME", "AWG_MESH_MIKROTIK_HOME_ENVS", "AWG_MESH_MIKROTIK_HOME_CONFIG"},
		{"office", "AWG_MESH_OFFICE", "AWG_MESH_OFFICE_ENVS", "AWG_MESH_OFFICE_CONFIG"},
	}
	for _, tt := range tests {
		name := DeriveContainerName(tt.input)
		if name != tt.wantName {
			t.Errorf("DeriveContainerName(%q) = %q, want %q", tt.input, name, tt.wantName)
		}
		if env := DeriveEnvListName(name); env != tt.wantEnv {
			t.Errorf("DeriveEnvListName(%q) = %q, want %q", name, env, tt.wantEnv)
		}
		if mount := DeriveMountName(name); mount != tt.wantMount {
			t.Errorf("DeriveMountName(%q) = %q, want %q", name, mount, tt.wantMount)
		}
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
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := escapeRouterOSToken(tt.input)
			if got != tt.want {
				t.Fatalf("escapeRouterOSToken(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
