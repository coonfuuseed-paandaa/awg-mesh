package cmd

import "testing"

func TestResolveImage(t *testing.T) {
	cases := []struct {
		name        string
		cliFlag     string
		topoDefault string
		fallback    string
		want        string
	}{
		{
			name:        "all-empty returns fallback",
			cliFlag:     "",
			topoDefault: "",
			fallback:    "fb",
			want:        "fb",
		},
		{
			name:        "cli-only returns cliFlag",
			cliFlag:     "cli",
			topoDefault: "",
			fallback:    "fb",
			want:        "cli",
		},
		{
			name:        "topo-only returns topoDefault",
			cliFlag:     "",
			topoDefault: "topo",
			fallback:    "fb",
			want:        "topo",
		},
		{
			name:        "cli overrides topo",
			cliFlag:     "cli",
			topoDefault: "topo",
			fallback:    "fb",
			want:        "cli",
		},
		{
			name:        "all-empty with empty fallback returns empty",
			cliFlag:     "",
			topoDefault: "",
			fallback:    "",
			want:        "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := resolveImage(tc.cliFlag, tc.topoDefault, tc.fallback)
			if got != tc.want {
				t.Errorf("resolveImage(%q, %q, %q) = %q, want %q",
					tc.cliFlag, tc.topoDefault, tc.fallback, got, tc.want)
			}
		})
	}
}
