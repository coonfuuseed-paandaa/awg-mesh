package awggen

import (
	"fmt"
	"strings"
)

// Preset defines AWG parameter generation ranges.
type Preset struct {
	Name           string
	JcRange        [2]int
	JminRange      [2]int
	JmaxExtraRange [2]int
	S1Range        [2]int
	S2Range        [2]int
	S3Range        [2]int
	S4Range        [2]int
	HMin           int64
	HMax           int64
	ISpecEnabled   bool
}

var presets = []Preset{
	{
		Name:           "Aggressive",
		JcRange:        [2]int{4, 8},
		JminRange:      [2]int{80, 130},
		JmaxExtraRange: [2]int{80, 200},
		S1Range:        [2]int{20, 63},
		S2Range:        [2]int{20, 63},
		S3Range:        [2]int{20, 63},
		S4Range:        [2]int{5, 15},
		HMin:           150_000_000,
		HMax:           2_000_000_000,
		ISpecEnabled:   true,
	},
	{
		Name:           "Balanced",
		JcRange:        [2]int{3, 6},
		JminRange:      [2]int{64, 113},
		JmaxExtraRange: [2]int{50, 149},
		S1Range:        [2]int{15, 50},
		S2Range:        [2]int{15, 50},
		S3Range:        [2]int{15, 50},
		S4Range:        [2]int{1, 10},
		HMin:           400_000_000,
		HMax:           1_600_000_000,
		ISpecEnabled:   true,
	},
	{
		Name:           "Minimal",
		JcRange:        [2]int{1, 3},
		JminRange:      [2]int{40, 80},
		JmaxExtraRange: [2]int{20, 80},
		S1Range:        [2]int{5, 20},
		S2Range:        [2]int{5, 20},
		S3Range:        [2]int{5, 20},
		S4Range:        [2]int{1, 5},
		HMin:           800_000_000,
		HMax:           1_200_000_000,
		ISpecEnabled:   false,
	},
}

// GetPreset returns a named generation preset.
func GetPreset(name string) (*Preset, error) {
	normalized := strings.TrimSpace(name)
	if normalized == "" {
		return nil, fmt.Errorf("preset name is required")
	}

	for _, preset := range presets {
		if strings.EqualFold(preset.Name, normalized) {
			selected := preset
			return &selected, nil
		}
	}

	return nil, fmt.Errorf("unknown preset: %q", name)
}

// ListPresets returns all available generation presets.
func ListPresets() []Preset {
	result := make([]Preset, len(presets))
	copy(result, presets)
	return result
}
