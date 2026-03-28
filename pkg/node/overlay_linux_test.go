//go:build linux

package node

import "testing"

func TestAssignOverlayIPRejectsInvalidIP(t *testing.T) {
	if err := AssignOverlayIP(""); err == nil {
		t.Fatalf("AssignOverlayIP() expected error for empty IP")
	}
	if err := AssignOverlayIP("not-an-ip"); err == nil {
		t.Fatalf("AssignOverlayIP() expected error for invalid IP")
	}
}

func TestRemoveOverlayIPRejectsInvalidIP(t *testing.T) {
	if err := RemoveOverlayIP(""); err == nil {
		t.Fatalf("RemoveOverlayIP() expected error for empty IP")
	}
	if err := RemoveOverlayIP("not-an-ip"); err == nil {
		t.Fatalf("RemoveOverlayIP() expected error for invalid IP")
	}
}
