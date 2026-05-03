package topology

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// DetectSchemaVersion inspects raw YAML bytes and reports the schema generation.
//
// Detection rules:
//   - schema_version: 2 → SchemaV2
//   - presence of `transport:` block OR `masters:` / `endpoints:` keys → SchemaV1 (v1.x)
//   - empty input or unparseable YAML → 0 + error
//
// This is implemented (not stub) because it is needed by `mesh-ctl topology
// validate` to route v1.x rejection vs v2.0 acceptance per F-009 FR-13.
func DetectSchemaVersion(in []byte) (SchemaVersion, error) {
	if len(in) == 0 {
		return 0, errors.New("topology: empty input")
	}
	// Use a permissive map to detect markers without forcing the strict struct shape.
	var raw map[string]any
	if err := yaml.Unmarshal(in, &raw); err != nil {
		return 0, fmt.Errorf("topology: parse YAML: %w", err)
	}
	if raw == nil {
		return 0, errors.New("topology: empty document")
	}
	if v, ok := raw["schema_version"]; ok {
		// Schema explicitly declared.
		switch x := v.(type) {
		case int:
			if x == 2 {
				return SchemaV2, nil
			}
			return 0, fmt.Errorf("topology: unsupported schema_version: %d", x)
		case int64:
			if x == 2 {
				return SchemaV2, nil
			}
			return 0, fmt.Errorf("topology: unsupported schema_version: %d", x)
		default:
			return 0, fmt.Errorf("topology: schema_version must be integer, got %T", v)
		}
	}
	// No schema_version declared — detect v1.x by structural markers.
	if _, hasMasters := raw["masters"]; hasMasters {
		return SchemaV1, nil
	}
	if _, hasEndpoints := raw["endpoints"]; hasEndpoints {
		return SchemaV1, nil
	}
	if t, hasTransport := raw["transport"]; hasTransport {
		// transport: block with pool/prefix_length is the v1.x marker.
		if m, ok := t.(map[string]any); ok {
			if _, hasPool := m["pool"]; hasPool {
				return SchemaV1, nil
			}
			if _, hasPrefix := m["prefix_length"]; hasPrefix {
				return SchemaV1, nil
			}
		}
	}
	// Has neither schema_version nor v1.x markers — likely partial / unrecognized.
	return 0, errors.New("topology: cannot determine schema version (no schema_version key and no v1.x markers)")
}
