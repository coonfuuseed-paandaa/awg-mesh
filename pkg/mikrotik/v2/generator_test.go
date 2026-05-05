package v2

import (
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

func TestGenerateStaticRSCProducesDeterministicNativeWireGuardScript(t *testing.T) {
	t.Parallel()

	topo := &topology.TopologyV2{
		SchemaVersion: topology.SchemaV2,
		Mesh: topology.MeshConfig{
			Name:            "test-mesh",
			OverlaySupernet: "172.21.92.0/24",
		},
		Nodes: []topology.NodeV2{
			{Name: "router-01", Roles: []role.Role{role.RoleClient}, Platform: "mikrotik", OverlayIP: "172.21.92.130"},
			{Name: "ingress-de", Roles: []role.Role{role.RoleIngress}, OverlayIP: "172.21.92.20"},
			{Name: "master-b", Roles: []role.Role{role.RoleMaster, role.RoleBalancer}, OverlayIP: "172.21.92.3", BridgeIP: "198.51.100.11"},
			{Name: "egress-us", Roles: []role.Role{role.RoleEgress}, OverlayIP: "172.21.92.34"},
			{Name: "master-a", Roles: []role.Role{role.RoleMaster, role.RoleBalancer}, OverlayIP: "172.21.92.2", PublicIP: "203.0.113.10", ClientProtocol: string(wg.ProtocolVanilla)},
		},
	}
	clientPrivate := testKey(10)
	masterKeys := map[string]wg.Key{
		"master-b": testKey(20),
		"master-a": testKey(30),
	}

	first, err := GenerateStaticRSC(topo, "router-01", clientPrivate, masterKeys)
	if err != nil {
		t.Fatalf("GenerateStaticRSC: %v", err)
	}
	second, err := GenerateStaticRSC(topo, "router-01", clientPrivate, masterKeys)
	if err != nil {
		t.Fatalf("GenerateStaticRSC second run: %v", err)
	}
	if first != second {
		t.Fatalf("script is not deterministic\nfirst:\n%s\nsecond:\n%s", first, second)
	}

	mustContain := []string{
		"# awg-mesh v2 RouterOS native WireGuard client script",
		"/interface/wireguard/add name=awg-mesh listen-port=13231 private-key=\"" + clientPrivate.String() + "\"",
		"/ip/address/add address=172.21.92.130/32 interface=awg-mesh",
		"public-key=\"" + masterKeys["master-a"].String() + "\" endpoint-address=203.0.113.10 endpoint-port=51820 allowed-address=172.21.92.2/32,172.21.92.34/32",
		"public-key=\"" + masterKeys["master-b"].String() + "\" endpoint-address=198.51.100.11 endpoint-port=51820 allowed-address=172.21.92.20/32,172.21.92.3/32",
		"persistent-keepalive=25s",
	}
	for _, want := range mustContain {
		if !strings.Contains(first, want) {
			t.Fatalf("script missing %q:\n%s", want, first)
		}
	}
	for _, forbidden := range []string{"amnezia", "jc=", "s1=", "h1=", "MESH_AWG"} {
		if strings.Contains(strings.ToLower(first), strings.ToLower(forbidden)) {
			t.Fatalf("native RouterOS script contains forbidden %q:\n%s", forbidden, first)
		}
	}
}

func TestGenerateStaticRSCRejectsMissingMasterKey(t *testing.T) {
	t.Parallel()

	topo := minimalTopology()
	_, err := GenerateStaticRSC(topo, "router-01", testKey(10), map[string]wg.Key{})
	if err == nil {
		t.Fatal("expected missing master key error")
	}
	if !strings.Contains(err.Error(), "master \"master-01\" public key is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGenerateStaticRSCRejectsNonVanillaMasterClientProtocol(t *testing.T) {
	t.Parallel()

	topo := minimalTopology()
	topo.Nodes[0].ClientProtocol = string(wg.ProtocolAmneziaWG)
	_, err := GenerateStaticRSC(topo, "router-01", testKey(10), map[string]wg.Key{"master-01": testKey(20)})
	if err == nil {
		t.Fatal("expected protocol rejection")
	}
	if !strings.Contains(err.Error(), "client_protocol must be \"vanilla-wg\"") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func minimalTopology() *topology.TopologyV2 {
	return &topology.TopologyV2{
		SchemaVersion: topology.SchemaV2,
		Mesh:          topology.MeshConfig{Name: "test-mesh", OverlaySupernet: "172.21.92.0/24"},
		Nodes: []topology.NodeV2{
			{Name: "master-01", Roles: []role.Role{role.RoleMaster}, OverlayIP: "172.21.92.2", PublicIP: "203.0.113.10"},
			{Name: "router-01", Roles: []role.Role{role.RoleClient}, Platform: "mikrotik", OverlayIP: "172.21.92.130"},
		},
	}
}

func testKey(seed byte) wg.Key {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	key, err := wg.NewKey(raw)
	if err != nil {
		panic(err)
	}
	return key
}
