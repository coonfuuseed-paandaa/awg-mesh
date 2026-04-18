package upgrade

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// SchemaVersion identifies the docker-compose schema version used by an awg-mesh node.
type SchemaVersion int

const (
	SchemaUnknown SchemaVersion = iota
	// Schema_v151: MESH_TOKEN (plain, not hashed) + command: flags for mode/name/IP.
	// Volume path: /var/lib/awg/{name} (non-namespaced).
	Schema_v151
	// Schema_v160: MESH_TOKEN_HASH + command: flags still present.
	// Volume path: /var/lib/awg-mesh/{name}.
	Schema_v160
	// Schema_v190: MESH_TOKEN_HASH, no command:, all config via environment:.
	// Missing MESH_CONFIG_DIR.
	Schema_v190
	// SchemaCurrent: MESH_TOKEN_HASH, no command:, MESH_CONFIG_DIR=/config.
	// Volume: /var/lib/awg-mesh/{name}:/config.
	SchemaCurrent
)

// String returns a human-readable schema version label.
func (s SchemaVersion) String() string {
	switch s {
	case Schema_v151:
		return "v1.5.1"
	case Schema_v160:
		return "v1.6.0"
	case Schema_v190:
		return "v1.9.0"
	case SchemaCurrent:
		return "current"
	default:
		return "unknown"
	}
}

// ParseSchemaVersion maps a user-supplied --from-schema string to a SchemaVersion.
// Accepted values: "v1.5.1", "v1.6.0", "v1.9.0", "current".
func ParseSchemaVersion(s string) (SchemaVersion, error) {
	switch strings.TrimSpace(s) {
	case "v1.5.1":
		return Schema_v151, nil
	case "v1.6.0":
		return Schema_v160, nil
	case "v1.9.0":
		return Schema_v190, nil
	case "current":
		return SchemaCurrent, nil
	default:
		return SchemaUnknown, fmt.Errorf("unknown schema version %q — accepted: v1.5.1, v1.6.0, v1.9.0, current", s)
	}
}

// composeDoc is a minimal parse target for heuristic detection.
// We only need the top-level services map; deep fields come from env/command.
type composeDoc struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Image       string            `yaml:"image"`
	Command     interface{}       `yaml:"command"` // may be string or []string
	Environment interface{}       `yaml:"environment"` // may be []string or map[string]string
	Volumes     []string          `yaml:"volumes"`
	Restart     string            `yaml:"restart"`
	NetworkMode string            `yaml:"network_mode"`
	CapAdd      []string          `yaml:"cap_add"`
	Devices     []string          `yaml:"devices"`
}

// DetectSchema heuristically identifies the schema version of a docker-compose
// YAML file used by awg-mesh, using ordered signals:
//
//  1. MESH_TOKEN (plain, not hashed) → Schema_v151
//  2. MESH_TOKEN_HASH + command: → Schema_v160
//  3. MESH_TOKEN_HASH + no command: + no MESH_CONFIG_DIR → Schema_v190
//  4. MESH_TOKEN_HASH + no command: + MESH_CONFIG_DIR → SchemaCurrent
//
// Use --from-schema to override if detection is ambiguous.
func DetectSchema(data []byte) (SchemaVersion, error) {
	if len(data) == 0 {
		return SchemaUnknown, fmt.Errorf("compose file is empty")
	}

	content := string(data)

	hasPlainToken := containsEnvKey(content, "MESH_TOKEN") && !containsEnvKey(content, "MESH_TOKEN_HASH")
	hasHashToken := containsEnvKey(content, "MESH_TOKEN_HASH")
	hasCommand := hasCommandBlock(data)
	hasConfigDir := containsEnvKey(content, "MESH_CONFIG_DIR")

	switch {
	case hasPlainToken:
		return Schema_v151, nil
	case hasHashToken && hasCommand:
		return Schema_v160, nil
	case hasHashToken && !hasCommand && !hasConfigDir:
		return Schema_v190, nil
	case hasHashToken && !hasCommand && hasConfigDir:
		return SchemaCurrent, nil
	default:
		return SchemaUnknown, fmt.Errorf(
			"cannot detect schema version: MESH_TOKEN_HASH=%v, command block=%v, MESH_CONFIG_DIR=%v — use --from-schema to override",
			hasHashToken, hasCommand, hasConfigDir,
		)
	}
}

// containsEnvKey returns true when the compose text contains a line that
// starts (after optional whitespace and "- ") with the given key followed by
// "=" or ":". This covers both list-style and map-style environment blocks.
func containsEnvKey(content, key string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		// List style: "- KEY=value" or "- KEY"
		trimmed = strings.TrimPrefix(trimmed, "- ")
		if strings.HasPrefix(trimmed, key+"=") || trimmed == key {
			return true
		}
		// Map style: "KEY: value"
		if strings.HasPrefix(trimmed, key+":") {
			return true
		}
	}
	return false
}

// hasCommandBlock returns true when the service has a non-empty command: field.
func hasCommandBlock(data []byte) bool {
	var doc composeDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		// Fall back to text search on parse failure.
		return strings.Contains(string(data), "command:")
	}
	for _, svc := range doc.Services {
		if svc.Command != nil {
			// command: null or command: [] counts as absent.
			switch v := svc.Command.(type) {
			case string:
				return strings.TrimSpace(v) != ""
			case []interface{}:
				return len(v) > 0
			}
		}
	}
	return false
}

// extractedFields holds the fields preserved from an old-schema compose.
type extractedFields struct {
	Image      string
	Name       string
	OverlayIP  string
	ListenPort string
	Mode       string
	TokenHash  string // empty when source had plain MESH_TOKEN
	PlainToken string // non-empty when source had plain MESH_TOKEN
	Restart    string
	Comments   []string // lines with operator comments, preserved
}

// MigrateCompose migrates a docker-compose YAML from the detected schema to
// the current schema, preserving node identity and transport state references.
//
// When fromSchema == SchemaCurrent, the input is returned byte-identical
// (idempotent migration).
//
// Returns (nil, error) when the input has data incompatible with the current
// layout. The error message explains what the operator must do manually.
func MigrateCompose(data []byte, fromSchema SchemaVersion) ([]byte, error) {
	if fromSchema == SchemaCurrent {
		// Idempotent — already current schema.
		result := make([]byte, len(data))
		copy(result, data)
		return result, nil
	}
	if fromSchema == SchemaUnknown {
		return nil, fmt.Errorf("cannot migrate unknown schema — use --from-schema to specify the source version")
	}

	fields, err := extractFields(data, fromSchema)
	if err != nil {
		return nil, err
	}

	return renderCurrentSchema(fields, fromSchema), nil
}

// extractFields parses the fields needed for migration from the source compose.
func extractFields(data []byte, fromSchema SchemaVersion) (*extractedFields, error) {
	content := string(data)
	fields := &extractedFields{}

	// Preserve operator comments.
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			fields.Comments = append(fields.Comments, line)
		}
	}

	// Extract restart policy (preserve verbatim).
	var doc composeDoc
	if err := yaml.Unmarshal(data, &doc); err == nil {
		for _, svc := range doc.Services {
			if svc.Restart != "" {
				fields.Restart = svc.Restart
			}
			if svc.Image != "" {
				fields.Image = svc.Image
			}
		}
	}
	if fields.Restart == "" {
		fields.Restart = "unless-stopped"
	}

	// Extract environment fields from the raw text (works for both list and map styles).
	// Strip surrounding quotes that YAML may preserve for numeric strings.
	fields.Name = stripQuotes(extractEnvValue(content, "MESH_NAME"))
	fields.OverlayIP = stripQuotes(extractEnvValue(content, "MESH_OVERLAY_IP"))
	fields.ListenPort = stripQuotes(extractEnvValue(content, "MESH_LISTEN_PORT"))
	fields.Mode = stripQuotes(extractEnvValue(content, "MESH_MODE"))
	fields.TokenHash = stripQuotes(extractEnvValue(content, "MESH_TOKEN_HASH"))
	fields.PlainToken = stripQuotes(extractEnvValue(content, "MESH_TOKEN"))

	// For older schemas (v1.5.1 and v1.6.0): node identity fields may come
	// from command: flags instead of the environment block.
	if fromSchema == Schema_v151 || fromSchema == Schema_v160 {
		if fields.Name == "" {
			fields.Name = stripQuotes(extractCommandFlag(content, "--name"))
		}
		if fields.OverlayIP == "" {
			fields.OverlayIP = stripQuotes(extractCommandFlag(content, "--overlay-ip"))
		}
		if fields.ListenPort == "" {
			fields.ListenPort = stripQuotes(extractCommandFlag(content, "--listen-port"))
		}
		if fields.Mode == "" {
			fields.Mode = stripQuotes(extractCommandFlag(content, "--mode"))
		}
	}

	// Validate required fields.
	if fields.Name == "" {
		return nil, fmt.Errorf(
			"cannot migrate compose: MESH_NAME (or --name flag) is missing — "+
				"add it manually to the new compose and re-run with --from-schema %s",
			fromSchema,
		)
	}

	return fields, nil
}

// extractEnvValue extracts the value of an environment variable from compose text.
// Handles both "- KEY=value" (list) and "KEY: value" (map) styles.
func extractEnvValue(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		// List style: "- KEY=value"
		withoutDash := strings.TrimPrefix(trimmed, "- ")
		if strings.HasPrefix(withoutDash, key+"=") {
			return strings.TrimPrefix(withoutDash, key+"=")
		}
		// Map style: "KEY: value"
		if strings.HasPrefix(trimmed, key+": ") {
			return strings.TrimPrefix(trimmed, key+": ")
		}
		if trimmed == key+":" {
			return ""
		}
	}
	return ""
}

// extractCommandFlag extracts a flag value from a command: block.
// Handles three patterns:
//
//  1. Inline: "command: awg-mesh-node --mode master --name mynode"
//  2. Same-item: "- --name=mynode" or "- --name mynode"
//  3. Split-item: consecutive list items "- --name\n      - mynode"
func extractCommandFlag(content, flag string) string {
	lines := strings.Split(content, "\n")
	inCommand := false

	// Collect command list items so we can look ahead for split-item pattern.
	var cmdItems []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "command:" || strings.HasPrefix(trimmed, "command: ") {
			inCommand = true
			// May have inline value: "command: awg-mesh-node --mode master ..."
			rest := strings.TrimPrefix(trimmed, "command:")
			rest = strings.TrimSpace(rest)
			if val := flagValueFromArgs(rest, flag); val != "" {
				return val
			}
			continue
		}
		if inCommand {
			if strings.HasPrefix(trimmed, "- ") {
				cmdItems = append(cmdItems, strings.TrimPrefix(trimmed, "- "))
			} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				// Non-list, non-comment line ends command block.
				inCommand = false
			}
		}
	}

	// Search collected items: same-item ("--flag=val" or "--flag val") and
	// split-item ("--flag" followed immediately by the value item).
	for i, item := range cmdItems {
		// Same-item: "--flag=value" or "--flag value"
		if val := flagValueFromArgs(item, flag); val != "" {
			return val
		}
		// Split-item: this item IS the flag, next item is the value.
		if strings.TrimSpace(item) == flag && i+1 < len(cmdItems) {
			next := strings.TrimSpace(cmdItems[i+1])
			if next != "" && !strings.HasPrefix(next, "-") {
				return next
			}
		}
	}
	return ""
}

// stripQuotes removes a single surrounding pair of double or single quotes from s.
// Used to normalise values like `"51820"` extracted from YAML-quoted scalars.
func stripQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// flagValueFromArgs searches args (space-separated or "--flag=value") for flag.
func flagValueFromArgs(args, flag string) string {
	parts := strings.Fields(args)
	for i, p := range parts {
		// "--flag=value" form.
		if strings.HasPrefix(p, flag+"=") {
			return strings.TrimPrefix(p, flag+"=")
		}
		// "--flag value" form.
		if p == flag && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// renderCurrentSchema produces the current-schema docker-compose content from
// the extracted fields.
func renderCurrentSchema(f *extractedFields, fromSchema SchemaVersion) []byte {
	var b strings.Builder

	// Write preserved operator comments at the top.
	for _, c := range f.Comments {
		b.WriteString(c)
		b.WriteByte('\n')
	}
	if len(f.Comments) > 0 {
		b.WriteByte('\n')
	}

	name := f.Name
	if name == "" {
		name = "unknown"
	}
	mode := f.Mode
	if mode == "" {
		mode = "endpoint"
	}
	image := f.Image
	if image == "" {
		image = "ghcr.io/coonfuuseed-paandaa/awg-mesh-node:latest"
	}
	restart := f.Restart

	b.WriteString("services:\n")
	b.WriteString("  awg-mesh-node:\n")
	b.WriteString("    image: " + image + "\n")
	b.WriteString("    network_mode: host\n")
	b.WriteString("    restart: " + restart + "\n")
	b.WriteString("    cap_add:\n")
	b.WriteString("      - NET_ADMIN\n")
	b.WriteString("      - NET_RAW\n")
	b.WriteString("    devices:\n")
	b.WriteString("      - /dev/net/tun:/dev/net/tun\n")
	b.WriteString("    environment:\n")
	b.WriteString("      # Bootstrap: written to /config/mesh.token on first boot, then ignored.\n")

	// Token hash line: preserve hash or emit TODO comment.
	if f.TokenHash != "" {
		b.WriteString("      - MESH_TOKEN_HASH=" + f.TokenHash + "\n")
	} else if f.PlainToken != "" {
		b.WriteString("      # TODO: verify — MESH_TOKEN_HASH must be a bcrypt hash of the token.\n")
		b.WriteString("      # The original compose used the plain MESH_TOKEN; replace with the hashed value.\n")
		b.WriteString("      # Run: mesh-ctl token hash <your-token>  to get the correct hash.\n")
		b.WriteString("      - MESH_TOKEN_HASH=REPLACE_WITH_HASH\n")
	} else {
		b.WriteString("      # TODO: verify — MESH_TOKEN_HASH is missing from the source compose.\n")
		b.WriteString("      - MESH_TOKEN_HASH=REPLACE_WITH_HASH\n")
	}

	b.WriteString("      - MESH_MODE=" + mode + "\n")
	b.WriteString("      - MESH_NAME=" + name + "\n")
	if f.OverlayIP != "" {
		b.WriteString("      - MESH_OVERLAY_IP=" + f.OverlayIP + "\n")
	} else {
		b.WriteString("      # TODO: verify — MESH_OVERLAY_IP missing from source compose.\n")
		b.WriteString("      - MESH_OVERLAY_IP=\n")
	}
	if f.ListenPort != "" {
		b.WriteString("      - MESH_LISTEN_PORT=" + f.ListenPort + "\n")
	} else {
		b.WriteString("      # TODO: verify — MESH_LISTEN_PORT missing from source compose.\n")
		b.WriteString("      - MESH_LISTEN_PORT=\n")
	}
	b.WriteString("      - MESH_CONFIG_DIR=/config\n")
	b.WriteString("    volumes:\n")
	b.WriteString("      - /var/lib/awg-mesh/" + name + ":/config\n")

	// Note the migration source for operator awareness.
	if fromSchema != SchemaCurrent {
		b.WriteString("    # Migrated from schema " + fromSchema.String() + " by mesh-ctl upgrade compose\n")
	}

	return []byte(b.String())
}
