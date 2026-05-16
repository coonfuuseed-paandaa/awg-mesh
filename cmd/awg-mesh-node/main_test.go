package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
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

func TestRunMasterDryRunAcceptsCoordinationListenerFlags(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "master",
		"--dry-run",
		"--name", "master-01",
		"--overlay-ip", "172.21.92.2",
		"--coordination-listen", "127.0.0.1:0",
		"--coordination-state-dir", stateDir,
		"--public-ip", "203.0.113.10",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"master dry-run node=master-01",
		"endpoint=203.0.113.10:51821",
		"coordination=127.0.0.1:0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output missing %q: %s", want, out)
		}
	}
}

func TestRunMasterDryRunAcceptsExplicitMeshEndpoint(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "master",
		"--dry-run",
		"--name", "master-01",
		"--overlay-ip", "172.21.92.2",
		"--mesh-listen-port", "443",
		"--public-ip", "203.0.113.10",
		"--mesh-endpoint", "mesh.example.test:8443",
		"--coordination-listen", "127.0.0.1:0",
		"--coordination-state-dir", stateDir,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "endpoint=mesh.example.test:8443") {
		t.Fatalf("explicit mesh endpoint missing from dry-run output: %s", out)
	}
	if strings.Contains(out, "endpoint=203.0.113.10:443") {
		t.Fatalf("explicit --mesh-endpoint must override --public-ip + --mesh-listen-port: %s", out)
	}
}

func TestRunMasterRejectsCoordinationWithoutAdvertisedEndpoint(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "master",
		"--dry-run",
		"--name", "master-01",
		"--overlay-ip", "172.21.92.2",
		"--coordination-listen", "127.0.0.1:0",
		"--coordination-state-dir", t.TempDir(),
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit without advertised endpoint; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "mesh endpoint is required") {
		t.Fatalf("expected mesh endpoint error, got stderr=%s", stderr.String())
	}
}

func TestRunMasterRejectsCoordinationOptionsWithoutListener(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "master",
		"--dry-run",
		"--name", "master-01",
		"--overlay-ip", "172.21.92.2",
		"--coordination-state-dir", t.TempDir(),
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit when coordination options omit listener; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--coordination-listen is required") {
		t.Fatalf("expected coordination-listen error, got stderr=%s", stderr.String())
	}
}

func TestControlPlaneModeRemainsCompatibilitySurface(t *testing.T) {
	t.Parallel()

	if _, ok := supportedModes["control-plane"]; !ok {
		t.Fatal("--mode control-plane must remain accepted in v2.0.1 compatibility surface")
	}
	var stderr strings.Builder
	warnDeprecatedMode("control-plane", &stderr)
	msg := stderr.String()
	for _, want := range []string{"compatibility", "deprecated", "master-owned coordination"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("control-plane compatibility warning missing %q: %s", want, msg)
		}
	}
}

func TestRunMasterDryRunAcceptsClientPrivateKeyFile(t *testing.T) {
	t.Parallel()

	keyPath := writeTestWGKey(t)
	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "master",
		"--dry-run",
		"--name", "master-01",
		"--overlay-ip", "172.21.92.2",
		"--client-private-key-file", keyPath,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), readFileTrimmed(t, keyPath)) {
		t.Fatalf("dry-run output leaked private key: %s", stdout.String())
	}
}

func TestRunMasterDryRunAcceptsClientPeer(t *testing.T) {
	t.Parallel()

	privateKey, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "master",
		"--dry-run",
		"--name", "master-01",
		"--overlay-ip", "172.21.92.2",
		"--client-peer", privateKey.PublicKey().String() + "=172.21.92.130/32",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d stderr=%s", code, stderr.String())
	}
}

func TestRunMasterDryRunRejectsInvalidClientPeer(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "master",
		"--dry-run",
		"--name", "master-01",
		"--overlay-ip", "172.21.92.2",
		"--client-peer", "not-a-key=172.21.92.130/32",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2, got %d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid public key") {
		t.Fatalf("expected public key parse error, got %s", stderr.String())
	}
}

func TestRunMasterDryRunRejectsInvalidClientPrivateKeyFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(keyPath, []byte("not-a-key\n"), 0o600); err != nil {
		t.Fatalf("write key fixture: %v", err)
	}
	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "master",
		"--dry-run",
		"--name", "master-01",
		"--overlay-ip", "172.21.92.2",
		"--client-private-key-file", keyPath,
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2, got %d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "parse client private key file") {
		t.Fatalf("expected parse error, got %s", stderr.String())
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

func writeTestWGKey(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	key, err := wg.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	path := filepath.Join(dir, "client-wg-private.key")
	if err := os.WriteFile(path, []byte(key.String()+"\n"), 0o600); err != nil {
		t.Fatalf("write key fixture: %v", err)
	}
	return path
}

func readFileTrimmed(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(data))
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

func TestRunBalancerDryRunUsesPolicyAndTargets(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "balancer",
		"--dry-run",
		"--name", "master-01",
		"--overlay-ip", "172.21.92.1",
		"--balancer-mode", "labeled",
		"--balancer-egress", "egress-ru=172.21.92.10:51821,weight=2",
		"--balancer-egress", "egress-eu=172.21.92.11:51821,weight=1",
		"--balancer-dscp", "10=egress-ru",
		"--balancer-fwmark", "100=egress-eu",
		"--balancer-health-interval", "2s",
		"--balancer-flow-idle-timeout", "9s",
		"--balancer-metrics", ":9093",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"balancer dry-run node=master-01",
		"overlay=172.21.92.1",
		"mode=labeled",
		"egresses=2",
		"health=2s",
		"flow_idle=9s",
		"metrics=:9093",
		"egress=egress-ru->172.21.92.10:51821/weight=2",
		"egress=egress-eu->172.21.92.11:51821/weight=1",
		"dscp=10->egress-ru",
		"fwmark=100->egress-eu",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output missing %q: %s", want, out)
		}
	}
}

func TestRunBalancerDryRunDiffersByPolicy(t *testing.T) {
	t.Parallel()

	var firstOut, firstErr strings.Builder
	firstCode := runCommand([]string{
		"--mode", "balancer",
		"--dry-run",
		"--name", "master-01",
		"--overlay-ip", "172.21.92.1",
		"--balancer-egress", "egress-ru=172.21.92.10:51821,weight=2",
	}, &firstOut, &firstErr)
	if firstCode != 0 {
		t.Fatalf("first dry-run exit %d stderr=%s", firstCode, firstErr.String())
	}

	var secondOut, secondErr strings.Builder
	secondCode := runCommand([]string{
		"--mode", "balancer",
		"--dry-run",
		"--name", "master-01",
		"--overlay-ip", "172.21.92.1",
		"--balancer-mode", "labeled",
		"--balancer-egress", "egress-eu=172.21.92.11:51821,weight=1",
		"--balancer-dscp", "10=egress-eu",
	}, &secondOut, &secondErr)
	if secondCode != 0 {
		t.Fatalf("second dry-run exit %d stderr=%s", secondCode, secondErr.String())
	}
	if firstOut.String() == secondOut.String() {
		t.Fatalf("dry-run output ignored mode/target/label: %s", firstOut.String())
	}
	if !strings.Contains(secondOut.String(), "mode=labeled") || !strings.Contains(secondOut.String(), "dscp=10->egress-eu") {
		t.Fatalf("second dry-run missing changed fields: %s", secondOut.String())
	}
}

func TestRunBalancerRejectsInvalidTarget(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "balancer",
		"--dry-run",
		"--name", "master-01",
		"--overlay-ip", "172.21.92.1",
		"--balancer-egress", "egress-ru=service.local:51821",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2, got %d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "must be an overlay IP") {
		t.Fatalf("expected target validation error, got %s", stderr.String())
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

func TestRunClientModeAcceptsCACertFlag(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certPath := filepath.Join(dir, "node.crt")
	keyPath := filepath.Join(dir, "node.key")
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(certPath, []byte("not-a-real-cert"), 0o600); err != nil {
		t.Fatalf("write cert fixture: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("not-a-real-key"), 0o600); err != nil {
		t.Fatalf("write key fixture: %v", err)
	}
	if err := os.WriteFile(caPath, []byte("not-a-real-ca"), 0o600); err != nil {
		t.Fatalf("write ca fixture: %v", err)
	}

	var stdout, stderr strings.Builder
	code := runCommand([]string{
		"--mode", "client",
		"--control-plane", "127.0.0.1:1",
		"--name", "client-a",
		"--overlay-ip", "172.21.92.130",
		"--region", "home",
		"--cert", certPath,
		"--key", keyPath,
		"--ca-cert", caPath,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected clientd runtime validation exit 1, got %d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("--ca-cert was not accepted by awg-mesh-node: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "load coordination target CA cert") {
		t.Fatalf("--ca-cert was not passed to clientd, stderr=%s", stderr.String())
	}
}
