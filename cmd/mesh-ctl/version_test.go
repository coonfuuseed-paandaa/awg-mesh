package main

import (
	"runtime/debug"
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

func TestVersionFromBuildInfoUsesSuiteVersionForLocalVCSBuild(t *testing.T) {
	got := versionFromBuildInfo(&debug.BuildInfo{
		Main: debug.Module{Version: "v1.14.2"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "1234567890abcdef"},
			{Key: "vcs.modified", Value: "false"},
		},
	}, true)

	want := awgmesh.Version + " (12345678)"
	if got != want {
		t.Fatalf("versionFromBuildInfo() = %q, want %q", got, want)
	}
}

func TestVersionFromBuildInfoPreservesModuleVersionWithoutVCS(t *testing.T) {
	got := versionFromBuildInfo(&debug.BuildInfo{
		Main: debug.Module{Version: "v1.14.2"},
	}, true)

	if got != "v1.14.2" {
		t.Fatalf("versionFromBuildInfo() = %q, want v1.14.2", got)
	}
}
