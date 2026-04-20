package cmd

import (
	"testing"

	proto "github.com/coonfuuseed-paandaa/awg-mesh/proto"
	"github.com/rs/zerolog"
)

func TestPortOffset(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		allMasters []string
		target     string
		want       int
	}{
		{
			name:       "N=1 single master",
			allMasters: []string{"mst-ru-01"},
			target:     "mst-ru-01",
			want:       0,
		},
		{
			name:       "N=2 unsorted input first alphabetical",
			allMasters: []string{"mst-ru-02", "mst-ru-01"},
			target:     "mst-ru-01",
			want:       0,
		},
		{
			name:       "N=2 unsorted input second alphabetical",
			allMasters: []string{"mst-ru-02", "mst-ru-01"},
			target:     "mst-ru-02",
			want:       1,
		},
		{
			name:       "N=3 alphabetical sort verified",
			allMasters: []string{"mst-ru-03", "mst-ru-01", "mst-ru-02"},
			target:     "mst-ru-03",
			want:       2,
		},
		{
			name:       "N=5 alphabetical sort verified",
			allMasters: []string{"mst-ru-05", "mst-ru-02", "mst-ru-04", "mst-ru-01", "mst-ru-03"},
			target:     "mst-ru-04",
			want:       3,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := portOffset(tc.allMasters, tc.target)
			if got != tc.want {
				t.Fatalf("portOffset(%v, %q) = %d, want %d", tc.allMasters, tc.target, got, tc.want)
			}
		})
	}
}

func TestComputePeerEndpoint(t *testing.T) {
	t.Parallel()

	masters := []string{"mst-ru-02", "mst-ru-01"}
	host := "ep.example.com"
	basePort := 443
	logger := zerolog.Nop()

	cases := []struct {
		name     string
		master   string
		respPort int32
		wantAddr string
	}{
		{name: "port-from-response master-A", master: "mst-ru-01", respPort: 443, wantAddr: "ep.example.com:443"},
		{name: "port-from-response master-B", master: "mst-ru-02", respPort: 444, wantAddr: "ep.example.com:444"},
		{name: "fallback master-A", master: "mst-ru-01", respPort: 0, wantAddr: "ep.example.com:443"},
		{name: "fallback master-B", master: "mst-ru-02", respPort: 0, wantAddr: "ep.example.com:444"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := computePeerEndpoint(
				host,
				basePort,
				&proto.AddPeerResponse{EndpointListenPort: tc.respPort},
				masters,
				tc.master,
				logger,
			)
			if got != tc.wantAddr {
				t.Fatalf("computePeerEndpoint(%q, %d, respPort=%d, masters=%v, master=%q) = %q, want %q",
					host, basePort, tc.respPort, masters, tc.master, got, tc.wantAddr)
			}
		})
	}
}
