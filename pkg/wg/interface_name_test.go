package wg

import "testing"

func TestValidateInterfaceName(t *testing.T) {
	t.Parallel()

	valid := []string{"w", "wg0", "awg-mesh0", "wg.test_1"}
	for _, name := range valid {
		t.Run("valid_"+name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateInterfaceName(name); err != nil {
				t.Fatalf("valid interface name rejected: %v", err)
			}
		})
	}

	invalid := []string{"", "0123456789abcdef", "wg/name", `wg\name`, "wg..name", "wg name", "wg:name"}
	for _, name := range invalid {
		t.Run("invalid_"+name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateInterfaceName(name); err == nil {
				t.Fatalf("invalid interface name %q accepted", name)
			}
		})
	}
}
