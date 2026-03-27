package awggen

import (
	"strings"
	"testing"
)

func TestGenerateParams(t *testing.T) {
	t.Parallel()

	_, err := GenerateParams(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "preset is required") {
		t.Fatalf("expected preset required error, got %v", err)
	}

	preset, err := GetPreset("Balanced")
	if err != nil {
		t.Fatalf("GetPreset returned error: %v", err)
	}
	family, err := GetFamily("TLSClientHello")
	if err != nil {
		t.Fatalf("GetFamily returned error: %v", err)
	}

	params, err := GenerateParams(preset, family)
	if err != nil {
		t.Fatalf("GenerateParams returned error: %v", err)
	}

	if params.Jc < preset.JcRange[0] || params.Jc > preset.JcRange[1] {
		t.Fatalf("Jc out of range: %d", params.Jc)
	}
	if params.Jmin < preset.JminRange[0] || params.Jmin > preset.JminRange[1] {
		t.Fatalf("Jmin out of range: %d", params.Jmin)
	}
	if params.Jmax < params.Jmin+preset.JmaxExtraRange[0] || params.Jmax > params.Jmin+preset.JmaxExtraRange[1] {
		t.Fatalf("Jmax out of expected range: Jmin=%d Jmax=%d", params.Jmin, params.Jmax)
	}
	if !isUnique(params.S1, params.S2, params.S3, params.S4) {
		t.Fatalf("S1-S4 must be unique: %d %d %d %d", params.S1, params.S2, params.S3, params.S4)
	}
	if params.I1 == "" || params.I2 == "" || params.I3 == "" || params.I4 == "" || params.I5 == "" {
		t.Fatalf("expected generated I-spec values for ISpec-enabled preset")
	}
}

func TestGenerateParamsWithoutISpec(t *testing.T) {
	t.Parallel()

	preset, err := GetPreset("Minimal")
	if err != nil {
		t.Fatalf("GetPreset returned error: %v", err)
	}
	family, err := GetFamily("TLSClientHello")
	if err != nil {
		t.Fatalf("GetFamily returned error: %v", err)
	}

	params, err := GenerateParams(preset, family)
	if err != nil {
		t.Fatalf("GenerateParams returned error: %v", err)
	}

	if params.I1 != "" || params.I2 != "" || params.I3 != "" || params.I4 != "" || params.I5 != "" {
		t.Fatalf("expected empty I-spec fields for ISpec-disabled preset, got %+v", params)
	}
}

func TestGenerateParamsInvalidPreset(t *testing.T) {
	t.Parallel()

	invalid := &Preset{
		Name:           "invalid",
		JcRange:        [2]int{2, 1},
		JminRange:      [2]int{1, 2},
		JmaxExtraRange: [2]int{1, 2},
		S1Range:        [2]int{1, 2},
		S2Range:        [2]int{1, 2},
		S3Range:        [2]int{1, 2},
		S4Range:        [2]int{1, 2},
		HMin:           1,
		HMax:           10,
	}

	_, err := GenerateParams(invalid, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid JcRange") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGenerateParamsFromCapture(t *testing.T) {
	t.Parallel()

	preset, err := GetPreset("Balanced")
	if err != nil {
		t.Fatalf("GetPreset returned error: %v", err)
	}

	_, err = GenerateParamsFromCapture(nil, []byte("abc"))
	if err == nil || !strings.Contains(err.Error(), "preset is required") {
		t.Fatalf("expected preset required error, got %v", err)
	}

	_, err = GenerateParamsFromCapture(preset, nil)
	if err == nil || !strings.Contains(err.Error(), "captureData must not be empty") {
		t.Fatalf("expected capture data error, got %v", err)
	}

	params, err := GenerateParamsFromCapture(preset, []byte("captured-template"))
	if err != nil {
		t.Fatalf("GenerateParamsFromCapture returned error: %v", err)
	}
	if params.I1 != "captured-template" {
		t.Fatalf("unexpected I1: %q", params.I1)
	}
	if params.I2 != "" || params.I3 != "" || params.I4 != "" || params.I5 != "" {
		t.Fatalf("expected I2-I5 to be empty")
	}
}

func TestParamsToConfig(t *testing.T) {
	t.Parallel()

	var nilParams *Params
	nilConfig := nilParams.ToConfig()
	if nilConfig.Jc != nil || nilConfig.H1 != nil {
		t.Fatalf("nil params should produce zero-value config")
	}

	params := &Params{
		Jc: 3, Jmin: 64, Jmax: 120,
		S1: 11, S2: 12, S3: 13, S4: 14,
		H1: 101, H2: 102, H3: 103, H4: 104,
		I1: "i1", I2: "i2", I3: "i3", I4: "i4", I5: "i5",
	}

	cfg := params.ToConfig()
	if cfg.Jc == nil || *cfg.Jc != 3 {
		t.Fatalf("unexpected Jc pointer: %#v", cfg.Jc)
	}
	if cfg.S4 == nil || *cfg.S4 != 14 {
		t.Fatalf("unexpected S4 pointer: %#v", cfg.S4)
	}
	if cfg.H3 == nil || *cfg.H3 != "103" {
		t.Fatalf("unexpected H3 pointer: %#v", cfg.H3)
	}
	if cfg.I5 == nil || *cfg.I5 != "i5" {
		t.Fatalf("unexpected I5 pointer: %#v", cfg.I5)
	}
}

func TestParamsToProto(t *testing.T) {
	t.Parallel()

	var nilParams *Params
	nilProto := nilParams.ToProto()
	if nilProto.Jc != 0 || nilProto.I1 != "" {
		t.Fatalf("nil params should produce zero-value proto")
	}

	params := &Params{
		Jc: 1, Jmin: 2, Jmax: 3,
		S1: 4, S2: 5, S3: 6, S4: 7,
		H1: 8, H2: 9, H3: 10, H4: 11,
		I1: "a", I2: "b", I3: "c", I4: "d", I5: "e",
	}

	pb := params.ToProto()
	if pb.Jc != 1 || pb.Jmin != 2 || pb.Jmax != 3 {
		t.Fatalf("unexpected jitter fields: %+v", pb)
	}
	if pb.S1 != 4 || pb.S4 != 7 {
		t.Fatalf("unexpected S fields: %+v", pb)
	}
	if pb.H1 != 8 || pb.H4 != 11 {
		t.Fatalf("unexpected H fields: %+v", pb)
	}
	if pb.I1 != "a" || pb.I5 != "e" {
		t.Fatalf("unexpected I fields: %+v", pb)
	}
}

func isUnique(values ...int) bool {
	seen := make(map[int]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}
