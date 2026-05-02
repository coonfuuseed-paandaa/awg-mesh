package main

import (
	"strings"
	"testing"
)

func TestRunMasterDryRunUsesDefaultDualListener(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "master",
		"--dry-run",
		"--name", "master-01",
		"--overlay-ip", "172.21.92.2",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"master dry-run node=master-01",
		"overlay=172.21.92.2",
		"client=wg-clients:51820/vanilla-wg",
		"mesh=wg-mesh:51821/amneziawg",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output missing %q: %s", want, out)
		}
	}
}

func TestRunMasterDryRunUsesCustomPorts(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "master",
		"--dry-run",
		"--name", "master-01",
		"--overlay-ip", "172.21.92.2",
		"--client-iface", "wg-clientx",
		"--mesh-iface", "wg-meshx",
		"--client-listen-port", "15182",
		"--mesh-listen-port", "15183",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "client=wg-clientx:15182/vanilla-wg") {
		t.Fatalf("custom client listener missing from dry-run output: %s", out)
	}
	if !strings.Contains(out, "mesh=wg-meshx:15183/amneziawg") {
		t.Fatalf("custom mesh listener missing from dry-run output: %s", out)
	}
}

func TestRunMasterDryRunRequiresIdentity(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := runCommand([]string{"--mode", "master", "--dry-run"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2, got %d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "master name is required") {
		t.Fatalf("expected validation error, got %s", stderr.String())
	}
}

func TestRunEgressDryRunUsesInternetInterface(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "egress",
		"--dry-run",
		"--name", "egress-01",
		"--overlay-ip", "172.21.92.20",
		"--internet-iface", "eth0",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"egress dry-run node=egress-01",
		"overlay=172.21.92.20",
		"internet=eth0",
		"nat=awg_mesh:nat_postrouting/oifname eth0 masquerade",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output missing %q: %s", want, out)
		}
	}
}

func TestRunEgressDryRunRejectsMeshInterface(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "egress",
		"--dry-run",
		"--name", "egress-01",
		"--overlay-ip", "172.21.92.20",
		"--internet-iface", "awg-mesh0",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2, got %d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "mesh interface") {
		t.Fatalf("expected mesh-interface validation error, got %s", stderr.String())
	}
}

func TestRunEgressNonDryRunValidatesClientdBeforeNAT(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "egress",
		"--name", "egress-01",
		"--overlay-ip", "172.21.92.20",
		"--internet-iface", "eth0",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2, got %d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "egress: clientd: missing required flags") {
		t.Fatalf("expected clientd validation error before NAT, got %s", stderr.String())
	}
}

func TestCompatibilityAliasesPreserved(t *testing.T) {
	t.Parallel()

	var endpointOut, endpointErr strings.Builder
	if code := runCommand([]string{
		"--mode", "endpoint",
		"--dry-run",
		"--name", "egress-01",
		"--overlay-ip", "172.21.92.20",
		"--internet-iface", "eth0",
	}, &endpointOut, &endpointErr); code != 0 {
		t.Fatalf("endpoint alias expected exit 0, got %d stderr=%s", code, endpointErr.String())
	}
	if !strings.Contains(endpointErr.String(), "warning: --mode endpoint is deprecated") {
		t.Fatalf("endpoint warning missing: %s", endpointErr.String())
	}
	if !strings.Contains(endpointOut.String(), "egress dry-run node=egress-01") {
		t.Fatalf("endpoint alias did not map to egress dry-run: %s", endpointOut.String())
	}

	var clientOut, clientErr strings.Builder
	if code := runCommand([]string{"--mode", "client"}, &clientOut, &clientErr); code != 2 {
		t.Fatalf("client alias expected exit 2 for missing flags, got %d stdout=%s stderr=%s", code, clientOut.String(), clientErr.String())
	}
	if !strings.Contains(clientErr.String(), "missing required flags") {
		t.Fatalf("client alias missing required-flags error: %s", clientErr.String())
	}
}
