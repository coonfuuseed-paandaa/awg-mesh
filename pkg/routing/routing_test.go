//go:build !linux

package routing

import (
	"testing"
)

func TestRoutingStubFunctionsReturnNotSupported(t *testing.T) {
	t.Parallel()

	errorText := "routing: not supported on this platform"
	hops := []NextHop{{Via: "10.0.0.1", Dev: "wg0", Weight: 1}}

	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "AddRoute",
			run: func() error {
				return AddRoute("10.0.0.0/24", "10.0.0.1", "wg0")
			},
		},
		{
			name: "DeleteRoute",
			run: func() error {
				return DeleteRoute("10.0.0.0/24")
			},
		},
		{
			name: "ReplaceRoute",
			run: func() error {
				return ReplaceRoute("10.0.0.0/24", "10.0.0.1", "wg0")
			},
		},
		{
			name: "ListRoutes",
			run: func() error {
				_, err := ListRoutes()
				return err
			},
		},
		{
			name: "SetECMPRoute",
			run: func() error {
				return SetECMPRoute("10.0.0.0/24", hops)
			},
		},
		{
			name: "RemoveECMPRoute",
			run: func() error {
				return RemoveECMPRoute("10.0.0.0/24")
			},
		},
		{
			name: "EnableMasquerade",
			run: func() error {
				return EnableMasquerade("wg0")
			},
		},
		{
			name: "DisableMasquerade",
			run: func() error {
				return DisableMasquerade("wg0")
			},
		},
		{
			name: "EnableForwarding",
			run: func() error {
				return EnableForwarding()
			},
		},
		{
			name: "ClampMSS",
			run: func() error {
				return ClampMSS("wg0", 1460)
			},
		},
		{
			name: "RemoveMSSClamp",
			run: func() error {
				return RemoveMSSClamp("wg0", 1460)
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := testCase.run()
			if err == nil {
				t.Fatal("expected not supported error")
			}
			if err.Error() != errorText {
				t.Fatalf("expected %q, got %q", errorText, err.Error())
			}
		})
	}
}

