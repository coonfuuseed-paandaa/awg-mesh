//go:build linux

package routing

import (
	"testing"

	"github.com/vishvananda/netlink"
)

func TestShouldCleanupDSCPRule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rule netlink.Rule
		want bool
	}{
		{
			name: "matching mark + priority + table in DSCP range",
			rule: netlink.Rule{
				Mark:     10,
				Priority: 110,
				Table:    110,
			},
			want: true,
		},
		{
			name: "lower-boundary mark/priority/table",
			rule: netlink.Rule{
				Mark:     1,
				Priority: 101,
				Table:    101,
			},
			want: true,
		},
		{
			name: "upper-boundary mark/priority/table",
			rule: netlink.Rule{
				Mark:     63,
				Priority: 163,
				Table:    163,
			},
			want: true,
		},
		{
			name: "mark below range",
			rule: netlink.Rule{
				Mark:     0,
				Priority: 110,
				Table:    110,
			},
			want: false,
		},
		{
			name: "mark above range",
			rule: netlink.Rule{
				Mark:     64,
				Priority: 110,
				Table:    110,
			},
			want: false,
		},
		{
			name: "priority below range",
			rule: netlink.Rule{
				Mark:     10,
				Priority: 100,
				Table:    110,
			},
			want: false,
		},
		{
			name: "priority above range",
			rule: netlink.Rule{
				Mark:     10,
				Priority: 164,
				Table:    110,
			},
			want: false,
		},
		{
			name: "foreign rule: mark and priority in DSCP range but table is main (254) — must NOT be deleted",
			rule: netlink.Rule{
				Mark:     10,
				Priority: 110,
				Table:    254,
			},
			want: false,
		},
		{
			name: "foreign rule: table below DSCP range",
			rule: netlink.Rule{
				Mark:     10,
				Priority: 110,
				Table:    100,
			},
			want: false,
		},
		{
			name: "foreign rule: table above DSCP range",
			rule: netlink.Rule{
				Mark:     10,
				Priority: 110,
				Table:    164,
			},
			want: false,
		},
		{
			name: "priority and table mismatch: table != 100+mark",
			rule: netlink.Rule{
				Mark:     10,
				Priority: 110,
				Table:    120,
			},
			want: false,
		},
		{
			name: "priority and table mismatch: priority != 100+mark",
			rule: netlink.Rule{
				Mark:     10,
				Priority: 120,
				Table:    110,
			},
			want: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldCleanupDSCPRule(tc.rule); got != tc.want {
				t.Fatalf("shouldCleanupDSCPRule(%+v) = %v, want %v", tc.rule, got, tc.want)
			}
		})
	}
}
