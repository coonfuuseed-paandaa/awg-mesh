package main

import "testing"

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
}
