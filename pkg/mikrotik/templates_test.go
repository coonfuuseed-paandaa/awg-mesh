package mikrotik

import (
	"strings"
	"testing"
)

func TestGenerateDeployRSC(t *testing.T) {
	t.Parallel()

	script, err := GenerateDeployRSC(DeployScript{
		TopologyName:  "mikrotik-home",
		ContainerName: "AWG_MESH_HOME",
		Image:         "ghcr.io/coonfuuseed-paandaa/awg-mesh-client:latest",
		Veth:          "AWG_MESH_HOME",
		VethGateway:   "100.127.0.1",
		OverlayIP:     "10.10.0.10",
		OverlayNet:    "10.10.0.0/16",
		TokenHash:     "$2a$12$abcdefghijklmnopqrstuv",
		DNS:           []string{"1.1.1.1", "8.8.8.8"},
		GRPCPort:      9090,
	})
	if err != nil {
		t.Fatalf("GenerateDeployRSC returned error: %v", err)
	}

	mustContain := []string{
		// Bridge
		"/interface/bridge/add name=BR_AWG_MESH",
		"/ip/address/add address=100.127.0.1/24 interface=BR_AWG_MESH",
		"/interface/bridge/port add bridge=BR_AWG_MESH interface=AWG_MESH_HOME",
		// Veth
		"/interface/veth add name=AWG_MESH_HOME",
		// NAT
		"/ip/firewall/nat add chain=srcnat action=masquerade",
		"awg-mesh: container masquerade",
		"/ip/firewall/nat add chain=dstnat protocol=tcp dst-port=9090",
		"awg-mesh: gRPC management port",
		// Firewall with place-before
		"place-before=",
		"connection-state=established,related",
		"awg-mesh: established return traffic",
		"awg-mesh: container outbound",
		// Route with comment
		"/ip/route add dst-address=10.10.0.0/16",
		"awg-mesh: overlay network",
		// Mount
		"/container/mounts/add name=AWG_MESH_HOME_CONFIG",
		"src=/docker/etc/awg-mesh-client-mikrotik-home-config",
		"dst=/config",
		// Container env vars (only essential ones)
		"key=MESH_TOKEN_HASH",
		"key=MESH_MODE",
		"key=MESH_NAME",
		"key=MESH_OVERLAY_IP",
		// Container add with production settings
		"/container/add interface=AWG_MESH_HOME",
		"root-dir=/docker/awg-mesh-client-mikrotik-home",
		"mounts=AWG_MESH_HOME_CONFIG",
		"dns=1.1.1.1,8.8.8.8",
		"logging=yes",
		"start-on-boot=yes",
		// Start
		"/container/start [find where name=AWG_MESH_HOME]",
	}

	mustNotContain := []string{
		"key=MESH_MASTERS",     // dead env var — node binary doesn't read it
		"key=MESH_AWG_CONFIG",  // dead duplicate of MESH_MASTERS
		"key=MESH_CONFIG_DIR",  // redundant — /config is the binary default
		"192.168.100",          // old default subnet — must be gone
	}

	// Plaintext MESH_TOKEN= must never land in a generated script.
	if strings.Contains(script, "key=MESH_TOKEN value=") {
		t.Fatalf("plaintext MESH_TOKEN= leaked into RouterOS script:\n%s", script)
	}

	for _, check := range mustContain {
		if !strings.Contains(script, check) {
			t.Errorf("expected script to contain %q, got:\n%s", check, script)
		}
	}
	for _, check := range mustNotContain {
		if strings.Contains(script, check) {
			t.Errorf("script must NOT contain %q, got:\n%s", check, script)
		}
	}

	// Ordering assertion: veth MUST be created before bridge-port references it.
	// RouterOS /import rejects bridge-port add if the veth interface does not yet exist.
	vethIdx := strings.Index(script, "/interface/veth add name=")
	bridgePortIdx := strings.Index(script, "/interface/bridge/port add")
	if vethIdx < 0 {
		t.Fatalf("script missing /interface/veth add")
	}
	if bridgePortIdx < 0 {
		t.Fatalf("script missing /interface/bridge/port add")
	}
	if vethIdx >= bridgePortIdx {
		t.Fatalf("veth section must precede bridge-port section: veth at %d, bridge/port at %d\nscript:\n%s", vethIdx, bridgePortIdx, script)
	}
}

func TestGenerateDeployRSCErrors(t *testing.T) {
	t.Parallel()

	base := DeployScript{
		TopologyName:  "test-client",
		ContainerName: "AWG_MESH_TEST",
		Image:         "img",
		Veth:          "AWG_MESH_TEST",
		OverlayIP:     "10.10.0.10",
		OverlayNet:    "10.10.0.0/16",
		TokenHash:     "$2a$12$abcdefghijklmnopqrstuv",
	}

	tests := []struct {
		name        string
		mutate      func(*DeployScript)
		expectError string
	}{
		{name: "missing container name", mutate: func(ds *DeployScript) { ds.ContainerName = "" }, expectError: "container name is required"},
		{name: "missing topology name", mutate: func(ds *DeployScript) { ds.TopologyName = "" }, expectError: "topology name is required"},
		{name: "missing image", mutate: func(ds *DeployScript) { ds.Image = "" }, expectError: "container image is required"},
		{name: "missing token hash", mutate: func(ds *DeployScript) { ds.TokenHash = "" }, expectError: "token hash is required"},
		{name: "invalid overlay ip", mutate: func(ds *DeployScript) { ds.OverlayIP = "bad-ip" }, expectError: "invalid overlay IP"},
		{name: "invalid overlay net", mutate: func(ds *DeployScript) { ds.OverlayNet = "bad-cidr" }, expectError: "invalid overlay network"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			current := base
			tt.mutate(&current)
			_, err := GenerateDeployRSC(current)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.expectError) {
				t.Fatalf("expected error containing %q, got %v", tt.expectError, err)
			}
		})
	}
}

func TestDeriveContainerName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"mikrotik-home", "AWG_MESH_MIKROTIK_HOME"},
		{"office", "AWG_MESH_OFFICE"},
		{"my-vpn-router", "AWG_MESH_MY_VPN_ROUTER"},
	}
	for _, tt := range tests {
		if got := DeriveContainerName(tt.input); got != tt.want {
			t.Errorf("DeriveContainerName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGenerateRotateRSC(t *testing.T) {
	t.Parallel()

	script, err := GenerateRotateRSC("AWG_MESH_HOME", RotateParams{
		Jc: 3, Jmin: 64, Jmax: 96,
		S1: 10, S2: 11,
		H1: 100, H2: 200, H3: 300, H4: 400,
	})
	if err != nil {
		t.Fatalf("GenerateRotateRSC returned error: %v", err)
	}

	checks := []string{
		"AWG_MESH_HOME_ENVS",
		"key=MESH_AWG_PARAMS",
		"jc=3,jmin=64,jmax=96,s1=10,s2=11,h1=100,h2=200,h3=300,h4=400",
		"/container/restart [find where name=\"AWG_MESH_HOME\"]",
	}
	for _, check := range checks {
		if !strings.Contains(script, check) {
			t.Fatalf("expected rotate script to contain %q, got:\n%s", check, script)
		}
	}
}

func TestGenerateRotateRSCErrors(t *testing.T) {
	t.Parallel()

	_, err := GenerateRotateRSC("", RotateParams{})
	if err == nil || !strings.Contains(err.Error(), "container name is required") {
		t.Fatalf("expected container-name error, got %v", err)
	}

	tests := []struct {
		name        string
		params      RotateParams
		expectError string
	}{
		{name: "negative field", params: RotateParams{Jc: -1}, expectError: "jc must be >= 0"},
		{name: "jmin greater than jmax", params: RotateParams{Jc: 1, Jmin: 10, Jmax: 5}, expectError: "must be <="},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := GenerateRotateRSC("container", tt.params)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.expectError) {
				t.Fatalf("expected error containing %q, got %v", tt.expectError, err)
			}
		})
	}
}
