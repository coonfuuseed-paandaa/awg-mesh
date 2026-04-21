package cmd

import (
	"fmt"
	"os"
	"strings"
)

// resolveImage returns the first non-empty string among cliFlag, topoDefault,
// and fallback, in that priority order. If all three are empty, it returns an
// empty string; the caller is responsible for supplying a non-empty fallback.
//
// topoKey names the topology field the caller is pulling the default from
// (e.g. "defaults.image.node", "defaults.image.client"). It appears only in
// the pinned-tag warning text so the operator knows exactly which topology
// stanza to edit.
//
// B15 fix: when the resolved image ends with a pinned semver tag (e.g. ":v1.10.0")
// that is older-looking than "latest", emit a warning to stderr so operators
// notice they are deploying from a stale pinned reference in their topology
// defaults. The warning fires only when the tag is a semver version string —
// bare "latest" and digest-pinned refs (containing "@sha256:") are silent.
func resolveImage(cliFlag, topoDefault, fallback, topoKey string) string {
	img := cliFlag
	if img == "" {
		img = topoDefault
	}
	if img == "" {
		img = fallback
	}

	// Warn when topology default supplies a pinned semver tag and the caller
	// did not override it via --image. CLI flag overrides are intentional;
	// topology defaults with pinned versions are commonly forgotten.
	if cliFlag == "" && img == topoDefault && isPinnedSemver(img) {
		key := topoKey
		if key == "" {
			key = "topology image default"
		}
		fmt.Fprintf(os.Stderr,
			"warning: topology %s is pinned to %q — pass --image or update topology to use a newer tag\n",
			key, img)
	}

	return img
}

// isPinnedSemver reports whether the image reference ends with a semver-style
// tag (":vX.Y.Z" or ":vX.Y.Z-suffix"). It returns false for ":latest",
// digest refs (@sha256:…), and refs without an explicit tag.
func isPinnedSemver(ref string) bool {
	if strings.Contains(ref, "@sha256:") {
		return false
	}
	colon := strings.LastIndex(ref, ":")
	if colon < 0 {
		return false // no tag at all
	}
	tag := ref[colon+1:]
	if tag == "latest" || tag == "" {
		return false
	}
	// Semver tags start with "v" followed by a digit.
	return len(tag) >= 2 && tag[0] == 'v' && tag[1] >= '0' && tag[1] <= '9'
}
