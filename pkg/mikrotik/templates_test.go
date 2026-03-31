package mikrotik

import (
	"strings"
	"testing"
)

func TestGenerateDeployRSC(t *testing.T) {
	t.Parallel()

	script, err := GenerateDeployRSC(DeployScript{
		ContainerName: "awg-client",
		Image:         "ghcr.io/coonfuuseed-paandaa/awg-mesh:latest",
		Veth:          "veth-awg",
		VethGateway:   "10.50.0.1",
		OverlayIP:     "10.10.0.10",
		OverlayNet:    "10.10.0.0/16",
		ListenPort:    51820,
		Masters:       []string{"master-a", "master-b"},
		AWGConfig:     "jc=3,jmin=64",
		Token:         "secure-token",
	})
	if err != nil {
		t.Fatalf("GenerateDeployRSC returned error: %v", err)
	}

	checks := []string{
		"# awg-mesh RouterOS deployment script",
		"/interface/veth add name=veth-awg",
		"/ip/route add dst-address=10.10.0.0/16",
		"/ip/firewall/filter add chain=forward in-interface=veth-awg action=accept",
		"/container/envs/add",
		"key=MESH_TOKEN",
		"key=MESH_MASTERS",
		"master-a,master-b",
		"/container/add interface=veth-awg",
		"/container/start [find where name=awg-client]",
	}
	for _, check := range checks {
		if !strings.Contains(script, check) {
			t.Fatalf("expected script to contain %q, got:\n%s", check, script)
		}
	}
}

func TestGenerateDeployRSCErrors(t *testing.T) {
	t.Parallel()

	base := DeployScript{
		ContainerName: "awg-client",
		Image:         "img",
		Veth:          "veth-awg",
		VethGateway:   "10.50.0.1",
		OverlayIP:     "10.10.0.10",
		OverlayNet:    "10.10.0.0/16",
		ListenPort:    51820,
		Masters:       []string{"master-a"},
		Token:         "token",
	}

	tests := []struct {
		name        string
		mutate      func(*DeployScript)
		expectError string
	}{
		{name: "missing container name", mutate: func(ds *DeployScript) { ds.ContainerName = "" }, expectError: "container name is required"},
		{name: "missing image", mutate: func(ds *DeployScript) { ds.Image = "" }, expectError: "container image is required"},
		{name: "missing token", mutate: func(ds *DeployScript) { ds.Token = "" }, expectError: "token is required"},
		{name: "invalid overlay ip", mutate: func(ds *DeployScript) { ds.OverlayIP = "bad-ip" }, expectError: "invalid overlay IP"},
		{name: "invalid overlay net", mutate: func(ds *DeployScript) { ds.OverlayNet = "bad-cidr" }, expectError: "invalid overlay network"},
		{name: "invalid gateway", mutate: func(ds *DeployScript) { ds.VethGateway = "bad-gateway" }, expectError: "invalid veth gateway"},
		{name: "invalid listen port", mutate: func(ds *DeployScript) { ds.ListenPort = 70000 }, expectError: "out of range"},
		{name: "empty masters", mutate: func(ds *DeployScript) { ds.Masters = nil }, expectError: "at least one master is required"},
		{name: "empty master entry", mutate: func(ds *DeployScript) { ds.Masters = []string{""} }, expectError: "empty entry"},
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

func TestGenerateRotateRSC(t *testing.T) {
	t.Parallel()

	script, err := GenerateRotateRSC("awg client", RotateParams{
		Jc: 3, Jmin: 64, Jmax: 96,
		S1: 10, S2: 11,
		H1: 100, H2: 200, H3: 300, H4: 400,
	})
	if err != nil {
		t.Fatalf("GenerateRotateRSC returned error: %v", err)
	}

	checks := []string{
		":local envList \"awg client-envs\"",
		"key=MESH_AWG_PARAMS",
		"jc=3,jmin=64,jmax=96,s1=10,s2=11,h1=100,h2=200,h3=300,h4=400",
		"/container/restart [find where name=\"awg client\"]",
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
