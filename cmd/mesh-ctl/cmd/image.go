package cmd

// resolveImage returns the first non-empty string among cliFlag, topoDefault,
// and fallback, in that priority order. If all three are empty, it returns an
// empty string; the caller is responsible for supplying a non-empty fallback.
func resolveImage(cliFlag, topoDefault, fallback string) string {
	if cliFlag != "" {
		return cliFlag
	}
	if topoDefault != "" {
		return topoDefault
	}
	return fallback
}
