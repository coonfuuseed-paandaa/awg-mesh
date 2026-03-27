package awggen

import (
	"fmt"
	"math/rand/v2"

	"github.com/thebtf/awg-mesh/pkg/wg"
	"github.com/thebtf/awg-mesh/proto"
)

const (
	maxPaddingGenerationAttempts = 4096
	maxS4GenerationAttempts      = 1024
	headerSegmentCount           = 4
)

// Params stores generated AmneziaWG obfuscation parameters.
type Params struct {
	Jc   int
	Jmin int
	Jmax int

	S1 int
	S2 int
	S3 int
	S4 int

	H1 int
	H2 int
	H3 int
	H4 int

	I1 string
	I2 string
	I3 string
	I4 string
	I5 string
}

// GenerateParams creates parameters using a preset and optional protocol family.
func GenerateParams(preset *Preset, family *ProtocolFamily) (*Params, error) {
	if preset == nil {
		return nil, fmt.Errorf("preset is required")
	}
	if err := validatePresetRanges(preset); err != nil {
		return nil, err
	}

	params, err := generateNumericParams(preset)
	if err != nil {
		return nil, err
	}

	if preset.ISpecEnabled && family != nil {
		iSpec, err := GenerateISpec(family)
		if err != nil {
			return nil, err
		}
		params.I1 = iSpec[0]
		params.I2 = iSpec[1]
		params.I3 = iSpec[2]
		params.I4 = iSpec[3]
		params.I5 = iSpec[4]
	}

	return params, nil
}

// GenerateParamsFromCapture creates parameters and uses captureData as I1 template.
func GenerateParamsFromCapture(preset *Preset, captureData []byte) (*Params, error) {
	if preset == nil {
		return nil, fmt.Errorf("preset is required")
	}
	if len(captureData) == 0 {
		return nil, fmt.Errorf("captureData must not be empty")
	}
	if err := validatePresetRanges(preset); err != nil {
		return nil, err
	}

	params, err := generateNumericParams(preset)
	if err != nil {
		return nil, err
	}

	params.I1 = string(captureData)
	params.I2 = ""
	params.I3 = ""
	params.I4 = ""
	params.I5 = ""

	return params, nil
}

// ToConfig converts generated params to wg.Config.
func (p *Params) ToConfig() wg.Config {
	if p == nil {
		return wg.Config{}
	}

	return wg.Config{
		Jc:   wg.IntPtr(p.Jc),
		Jmin: wg.IntPtr(p.Jmin),
		Jmax: wg.IntPtr(p.Jmax),
		S1:   wg.IntPtr(p.S1),
		S2:   wg.IntPtr(p.S2),
		S3:   wg.IntPtr(p.S3),
		S4:   wg.IntPtr(p.S4),
		H1:   wg.StrPtr(fmt.Sprintf("%d", p.H1)),
		H2:   wg.StrPtr(fmt.Sprintf("%d", p.H2)),
		H3:   wg.StrPtr(fmt.Sprintf("%d", p.H3)),
		H4:   wg.StrPtr(fmt.Sprintf("%d", p.H4)),
		I1:   wg.StrPtr(p.I1),
		I2:   wg.StrPtr(p.I2),
		I3:   wg.StrPtr(p.I3),
		I4:   wg.StrPtr(p.I4),
		I5:   wg.StrPtr(p.I5),
	}
}

// ToProto converts generated params to protobuf AwgParams.
func (p *Params) ToProto() *proto.AwgParams {
	if p == nil {
		return &proto.AwgParams{}
	}

	return &proto.AwgParams{
		Jc:   int32(p.Jc),
		Jmin: int32(p.Jmin),
		Jmax: int32(p.Jmax),
		S1:   int32(p.S1),
		S2:   int32(p.S2),
		S3:   int32(p.S3),
		S4:   int32(p.S4),
		H1:   int32(p.H1),
		H2:   int32(p.H2),
		H3:   int32(p.H3),
		H4:   int32(p.H4),
		I1:   p.I1,
		I2:   p.I2,
		I3:   p.I3,
		I4:   p.I4,
		I5:   p.I5,
	}
}

func validatePresetRanges(preset *Preset) error {
	if err := validateRange(preset.JcRange, "JcRange"); err != nil {
		return err
	}
	if err := validateRange(preset.JminRange, "JminRange"); err != nil {
		return err
	}
	if err := validateRange(preset.JmaxExtraRange, "JmaxExtraRange"); err != nil {
		return err
	}
	if err := validateRange(preset.S1Range, "S1Range"); err != nil {
		return err
	}
	if err := validateRange(preset.S2Range, "S2Range"); err != nil {
		return err
	}
	if err := validateRange(preset.S3Range, "S3Range"); err != nil {
		return err
	}
	if err := validateRange(preset.S4Range, "S4Range"); err != nil {
		return err
	}
	if preset.HMin > preset.HMax {
		return fmt.Errorf("invalid H range: HMin (%d) > HMax (%d)", preset.HMin, preset.HMax)
	}
	if preset.HMax-preset.HMin+1 < headerSegmentCount {
		return fmt.Errorf("H range must contain at least %d values: [%d, %d]", headerSegmentCount, preset.HMin, preset.HMax)
	}

	return nil
}

func validateRange(value [2]int, fieldName string) error {
	if value[0] > value[1] {
		return fmt.Errorf("invalid %s: min (%d) > max (%d)", fieldName, value[0], value[1])
	}
	return nil
}

func generateNumericParams(preset *Preset) (*Params, error) {
	jc, err := randomIntInRange(preset.JcRange[0], preset.JcRange[1])
	if err != nil {
		return nil, fmt.Errorf("failed to generate Jc: %w", err)
	}

	jmin, err := randomIntInRange(preset.JminRange[0], preset.JminRange[1])
	if err != nil {
		return nil, fmt.Errorf("failed to generate Jmin: %w", err)
	}

	jmaxExtra, err := randomIntInRange(preset.JmaxExtraRange[0], preset.JmaxExtraRange[1])
	if err != nil {
		return nil, fmt.Errorf("failed to generate Jmax extra: %w", err)
	}

	s1, s2, s3, s4, err := generatePaddingValues(preset)
	if err != nil {
		return nil, err
	}

	headers, err := generateHeaderValues(preset.HMin, preset.HMax)
	if err != nil {
		return nil, err
	}

	return &Params{
		Jc:   jc,
		Jmin: jmin,
		Jmax: jmin + jmaxExtra,
		S1:   s1,
		S2:   s2,
		S3:   s3,
		S4:   s4,
		H1:   headers[0],
		H2:   headers[1],
		H3:   headers[2],
		H4:   headers[3],
		I1:   "",
		I2:   "",
		I3:   "",
		I4:   "",
		I5:   "",
	}, nil
}

func generatePaddingValues(preset *Preset) (int, int, int, int, error) {
	for attempt := 0; attempt < maxPaddingGenerationAttempts; attempt++ {
		s1, err := randomIntInRange(preset.S1Range[0], preset.S1Range[1])
		if err != nil {
			return 0, 0, 0, 0, fmt.Errorf("failed to generate S1: %w", err)
		}
		s2, err := randomIntInRange(preset.S2Range[0], preset.S2Range[1])
		if err != nil {
			return 0, 0, 0, 0, fmt.Errorf("failed to generate S2: %w", err)
		}
		s3, err := randomIntInRange(preset.S3Range[0], preset.S3Range[1])
		if err != nil {
			return 0, 0, 0, 0, fmt.Errorf("failed to generate S3: %w", err)
		}

		if !allUnique(s1, s2, s3) || violatesPacketSizeRules(s1, s2, s3) {
			continue
		}

		s4, ok, err := generateUniqueS4(preset.S4Range, s1, s2, s3)
		if err != nil {
			return 0, 0, 0, 0, err
		}
		if !ok {
			continue
		}

		return s1, s2, s3, s4, nil
	}

	return 0, 0, 0, 0, fmt.Errorf("failed to generate valid S1-S4 after %d attempts", maxPaddingGenerationAttempts)
}

func allUnique(values ...int) bool {
	seen := make(map[int]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func violatesPacketSizeRules(s1, s2, s3 int) bool {
	if s1+148 == s2+92 {
		return true
	}
	if s3+64 == s1+148 {
		return true
	}
	return s3+64 == s2+92
}

func generateUniqueS4(s4Range [2]int, s1, s2, s3 int) (int, bool, error) {
	for attempt := 0; attempt < maxS4GenerationAttempts; attempt++ {
		s4, err := randomIntInRange(s4Range[0], s4Range[1])
		if err != nil {
			return 0, false, fmt.Errorf("failed to generate S4: %w", err)
		}
		if s4 != s1 && s4 != s2 && s4 != s3 {
			return s4, true, nil
		}
	}

	return 0, false, nil
}

func generateHeaderValues(hMin, hMax int64) ([4]int, error) {
	var values [4]int
	segments := splitHeaderRange(hMin, hMax)
	randomized := make([]int, 0, len(segments))
	for _, segment := range segments {
		value, err := randomInt64InRange(segment[0], segment[1])
		if err != nil {
			return [4]int{}, fmt.Errorf("failed to generate header value: %w", err)
		}
		randomized = append(randomized, int(value))
	}

	rand.Shuffle(len(randomized), func(i, j int) {
		randomized[i], randomized[j] = randomized[j], randomized[i]
	})
	copy(values[:], randomized)
	return values, nil
}

func splitHeaderRange(hMin, hMax int64) [][2]int64 {
	totalWidth := hMax - hMin + 1
	baseWidth := totalWidth / headerSegmentCount
	remainder := totalWidth % headerSegmentCount

	segments := make([][2]int64, 0, headerSegmentCount)
	rangeStart := hMin
	for index := 0; index < headerSegmentCount; index++ {
		currentWidth := baseWidth
		if int64(index) < remainder {
			currentWidth++
		}

		rangeEnd := rangeStart + currentWidth - 1
		segments = append(segments, [2]int64{rangeStart, rangeEnd})
		rangeStart = rangeEnd + 1
	}

	return segments
}

func randomIntInRange(minimum, maximum int) (int, error) {
	if minimum > maximum {
		return 0, fmt.Errorf("invalid range [%d, %d]", minimum, maximum)
	}
	return minimum + rand.IntN(maximum-minimum+1), nil
}

func randomInt64InRange(minimum, maximum int64) (int64, error) {
	if minimum > maximum {
		return 0, fmt.Errorf("invalid range [%d, %d]", minimum, maximum)
	}
	return minimum + rand.Int64N(maximum-minimum+1), nil
}
