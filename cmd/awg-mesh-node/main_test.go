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

func TestRunIngressDryRunUsesRouteAndPublicAddress(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "ingress",
		"--dry-run",
		"--name", "ingress-01",
		"--overlay-ip", "172.21.92.30",
		"--ingress-public-addr", ":8443",
		"--ingress-route", "media.example.com=172.21.92.10:8096",
		"--ingress-tenant", "tenant-a",
		"--ingress-health-interval", "2s",
		"--ingress-udp-idle-timeout", "9s",
		"--ingress-metrics", ":9092",
		"--ingress-acme-cache", "/var/lib/awg-mesh/acme",
		"--ingress-http3",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"ingress dry-run node=ingress-01",
		"overlay=172.21.92.30",
		"public=:8443",
		"routes=1",
		"health=2s",
		"udp_idle=9s",
		"tls=acme",
		"http3=true",
		"metrics=:9092",
		"route=tenant-a:media.example.com->172.21.92.10:8096/tls_terminate",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output missing %q: %s", want, out)
		}
	}
}

func TestRunIngressDryRunDiffersByRoute(t *testing.T) {
	t.Parallel()

	var firstOut, firstErr strings.Builder
	firstCode := runCommand([]string{
		"--mode", "ingress",
		"--dry-run",
		"--name", "ingress-01",
		"--overlay-ip", "172.21.92.30",
		"--ingress-public-addr", ":8443",
		"--ingress-route", "media.example.com=172.21.92.10:8096",
	}, &firstOut, &firstErr)
	if firstCode != 0 {
		t.Fatalf("first dry-run exit %d stderr=%s", firstCode, firstErr.String())
	}

	var secondOut, secondErr strings.Builder
	secondCode := runCommand([]string{
		"--mode", "ingress",
		"--dry-run",
		"--name", "ingress-01",
		"--overlay-ip", "172.21.92.30",
		"--ingress-public-addr", ":9443",
		"--ingress-route", "api.example.com=172.21.92.11:8080",
	}, &secondOut, &secondErr)
	if secondCode != 0 {
		t.Fatalf("second dry-run exit %d stderr=%s", secondCode, secondErr.String())
	}
	if firstOut.String() == secondOut.String() {
		t.Fatalf("dry-run output ignored route/public address: %s", firstOut.String())
	}
	if !strings.Contains(secondOut.String(), "public=:9443") || !strings.Contains(secondOut.String(), "api.example.com->172.21.92.11:8080") {
		t.Fatalf("second dry-run missing changed fields: %s", secondOut.String())
	}
}

func TestRunIngressRejectsInvalidRoute(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "ingress",
		"--dry-run",
		"--name", "ingress-01",
		"--overlay-ip", "172.21.92.30",
		"--ingress-public-addr", ":8443",
		"--ingress-route", "media.example.com",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2, got %d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "must use hostname=overlay_ip:port") {
		t.Fatalf("expected route validation error, got %s", stderr.String())
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
