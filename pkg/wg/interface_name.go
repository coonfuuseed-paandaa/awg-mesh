package wg

import "fmt"

const maxInterfaceNameLength = 15

func ValidateInterfaceName(name string) error {
	if name == "" {
		return fmt.Errorf("interface name is required")
	}
	if len(name) > maxInterfaceNameLength {
		return fmt.Errorf("interface name %q exceeds %d characters", name, maxInterfaceNameLength)
	}
	if containsDotDot(name) {
		return fmt.Errorf("interface name %q must not contain ..", name)
	}
	for _, r := range name {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' {
			continue
		}
		return fmt.Errorf("interface name %q contains invalid character %q", name, r)
	}
	return nil
}

func containsDotDot(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] == '.' && s[i] == '.' {
			return true
		}
	}
	return false
}
