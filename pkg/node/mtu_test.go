package node

import "testing"

func TestCalculateMTU(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		physicalMTU int
		awgOverhead int
		hops        int
		want        int
	}{
		{name: "single hop", physicalMTU: 1500, awgOverhead: 80, hops: 1, want: 1420},
		{name: "two hops", physicalMTU: 1500, awgOverhead: 80, hops: 2, want: 1340},
		{name: "clamped to minimum", physicalMTU: 1300, awgOverhead: 40, hops: 1, want: MinMTU},
		{name: "negative result clamped", physicalMTU: 500, awgOverhead: 500, hops: 2, want: MinMTU},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := CalculateMTU(tt.physicalMTU, tt.awgOverhead, tt.hops)
			if got != tt.want {
				t.Fatalf("CalculateMTU(%d, %d, %d) = %d, want %d", tt.physicalMTU, tt.awgOverhead, tt.hops, got, tt.want)
			}
		})
	}
}
