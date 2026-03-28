//go:build !linux

package node

import "testing"

func TestAssignOverlayIPNotImplementedPlatform(t *testing.T) {
	if err := AssignOverlayIP("10.77.0.10"); err != nil {
		t.Fatalf("AssignOverlayIP() error = %v", err)
	}
}

func TestRemoveOverlayIPNotImplementedPlatform(t *testing.T) {
	if err := RemoveOverlayIP("10.77.0.10"); err != nil {
		t.Fatalf("RemoveOverlayIP() error = %v", err)
	}
}
