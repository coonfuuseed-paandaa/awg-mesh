package proto

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"
)

// G14 v1.12.9: wire round-trip gate for AddTunnelRequest.AllowedIps (field 13).
// Prevents silent descriptor/struct drift. If the raw descriptor drops field 13,
// proto.Unmarshal returns an empty slice despite the struct tag. This test
// exercises the real wire path (Marshal -> Unmarshal) — would have caught
// local tracker #147 layer 4 (v1.12.8 failure) at CI time.
func TestAddTunnelRequest_AllowedIpsWireRoundtrip(t *testing.T) {
	in := &AddTunnelRequest{
		Name:                "test-tunnel",
		EndpointHost:        "1.2.3.4:443",
		OverlayIp:           "172.20.70.34",
		BalancerIp:          "172.20.70.33",
		TransportSubnet:     "10.255.0.24/30",
		MasterTransportIp:   "10.255.0.25",
		EndpointTransportIp: "10.255.0.26",
		Weight:              1,
		AllowedIps: []string{
			"10.255.0.24/30",
			"172.20.70.34/32",
			"172.20.70.32/27",
		},
	}
	data, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := &AddTunnelRequest{}
	if err := proto.Unmarshal(data, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out.AllowedIps, in.AllowedIps) {
		t.Fatalf("wire round-trip failed:\n  IN:  %v\n  OUT: %v\n  (descriptor likely missing field 13)",
			in.AllowedIps, out.AllowedIps)
	}
	// Also assert other core fields round-tripped — sanity check the base path.
	if out.Name != in.Name || out.OverlayIp != in.OverlayIp {
		t.Fatalf("base fields lost: name=%q overlay=%q", out.Name, out.OverlayIp)
	}
}
