package main

import (
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/awgmesh"
)

func TestVersionFromBuildOverride(t *testing.T) {
	orig := versionFromBuild
	t.Cleanup(func() { versionFromBuild = orig })

	versionFromBuild = "v9.9.9-test"
	got := version()
	if got != "v9.9.9-test" {
		t.Fatalf("versionFromBuild override not honored: got %q, want %q", got, "v9.9.9-test")
	}
}

func TestVersionFallbackWhenEmpty(t *testing.T) {
	orig := versionFromBuild
	t.Cleanup(func() { versionFromBuild = orig })

	versionFromBuild = ""
	got := version()
	if got == "v9.9.9-test" {
		t.Fatalf("version() returned the test-override value in fallback path: %q", got)
	}
	if !strings.HasPrefix(got, awgmesh.Version) {
		t.Fatalf("version() fallback = %q, want prefix %q", got, awgmesh.Version)
	}
}
