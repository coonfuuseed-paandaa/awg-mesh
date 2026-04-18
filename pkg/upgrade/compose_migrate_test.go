package upgrade

import (
	"strings"
	"testing"
)

// ─── schema fixtures ─────────────────────────────────────────────────────────

const composeV151 = `services:
  awg-mesh-node:
    image: ghcr.io/example/awg-mesh-node:v1.5.1
    network_mode: host
    restart: unless-stopped
    cap_add:
      - NET_ADMIN
    command:
      - awg-mesh-node
      - --mode
      - master
      - --name
      - node-a
      - --overlay-ip
      - 10.0.0.1/24
      - --listen-port
      - "51820"
    environment:
      - MESH_TOKEN=plaintexttoken123
    volumes:
      - /var/lib/awg/node-a:/data
`

const composeV160 = `services:
  awg-mesh-node:
    image: ghcr.io/example/awg-mesh-node:v1.6.0
    network_mode: host
    restart: always
    cap_add:
      - NET_ADMIN
    command:
      - awg-mesh-node
      - --mode
      - endpoint
      - --name
      - node-b
      - --overlay-ip
      - 10.0.0.2/24
      - --listen-port
      - "51821"
    environment:
      - MESH_TOKEN_HASH=$2b$10$examplehashvalue
    volumes:
      - /var/lib/awg-mesh/node-b:/data
`

const composeV190 = `services:
  awg-mesh-node:
    image: ghcr.io/example/awg-mesh-node:v1.9.0
    network_mode: host
    restart: unless-stopped
    cap_add:
      - NET_ADMIN
    environment:
      - MESH_TOKEN_HASH=$2b$10$examplehashvalue
      - MESH_MODE=master
      - MESH_NAME=node-c
      - MESH_OVERLAY_IP=10.0.0.3/24
      - MESH_LISTEN_PORT=51822
    volumes:
      - /var/lib/awg-mesh/node-c:/data
`

const composeCurrent = `services:
  awg-mesh-node:
    image: ghcr.io/example/awg-mesh-node:v1.10.0
    network_mode: host
    restart: unless-stopped
    cap_add:
      - NET_ADMIN
    environment:
      - MESH_TOKEN_HASH=$2b$10$examplehashvalue
      - MESH_MODE=master
      - MESH_NAME=node-d
      - MESH_OVERLAY_IP=10.0.0.4/24
      - MESH_LISTEN_PORT=51823
      - MESH_CONFIG_DIR=/config
    volumes:
      - /var/lib/awg-mesh/node-d:/config
`

// ─── DetectSchema ─────────────────────────────────────────────────────────────

func TestDetectSchema_v151(t *testing.T) {
	got, err := DetectSchema([]byte(composeV151))
	if err != nil {
		t.Fatalf("DetectSchema v151: %v", err)
	}
	if got != Schema_v151 {
		t.Errorf("got %v want Schema_v151", got)
	}
}

func TestDetectSchema_v160(t *testing.T) {
	got, err := DetectSchema([]byte(composeV160))
	if err != nil {
		t.Fatalf("DetectSchema v160: %v", err)
	}
	if got != Schema_v160 {
		t.Errorf("got %v want Schema_v160", got)
	}
}

func TestDetectSchema_v190(t *testing.T) {
	got, err := DetectSchema([]byte(composeV190))
	if err != nil {
		t.Fatalf("DetectSchema v190: %v", err)
	}
	if got != Schema_v190 {
		t.Errorf("got %v want Schema_v190", got)
	}
}

func TestDetectSchema_Current(t *testing.T) {
	got, err := DetectSchema([]byte(composeCurrent))
	if err != nil {
		t.Fatalf("DetectSchema current: %v", err)
	}
	if got != SchemaCurrent {
		t.Errorf("got %v want SchemaCurrent", got)
	}
}

func TestDetectSchema_Empty(t *testing.T) {
	_, err := DetectSchema([]byte{})
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

// ─── ParseSchemaVersion ───────────────────────────────────────────────────────

func TestParseSchemaVersion(t *testing.T) {
	cases := []struct {
		input string
		want  SchemaVersion
	}{
		{"v1.5.1", Schema_v151},
		{"v1.6.0", Schema_v160},
		{"v1.9.0", Schema_v190},
		{"current", SchemaCurrent},
	}
	for _, tc := range cases {
		got, err := ParseSchemaVersion(tc.input)
		if err != nil {
			t.Errorf("ParseSchemaVersion(%q): %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSchemaVersion(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestParseSchemaVersion_Unknown(t *testing.T) {
	_, err := ParseSchemaVersion("v2.0.0")
	if err == nil {
		t.Fatal("expected error for unknown version")
	}
}

// ─── MigrateCompose — idempotent for current ──────────────────────────────────

func TestMigrateCompose_CurrentIsIdempotent(t *testing.T) {
	data := []byte(composeCurrent)
	got, err := MigrateCompose(data, SchemaCurrent)
	if err != nil {
		t.Fatalf("MigrateCompose(current): %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("idempotent check failed:\ngot:\n%s\nwant:\n%s", got, data)
	}
}

// ─── MigrateCompose — schema transitions ──────────────────────────────────────

func TestMigrateCompose_v151_to_Current(t *testing.T) {
	got, err := MigrateCompose([]byte(composeV151), Schema_v151)
	if err != nil {
		t.Fatalf("MigrateCompose v151→current: %v", err)
	}
	out := string(got)

	// Name extracted from --name flag.
	assertContains(t, out, "MESH_NAME=node-a", "node name")
	// Mode extracted from --mode flag.
	assertContains(t, out, "MESH_MODE=master", "node mode")
	// Overlay IP extracted from --overlay-ip flag.
	assertContains(t, out, "MESH_OVERLAY_IP=10.0.0.1/24", "overlay ip")
	// Listen port extracted from --listen-port flag.
	assertContains(t, out, "MESH_LISTEN_PORT=51820", "listen port")
	// MESH_CONFIG_DIR must be present in output.
	assertContains(t, out, "MESH_CONFIG_DIR=/config", "config dir")
	// Plain token → TODO comment + placeholder hash.
	assertContains(t, out, "MESH_TOKEN_HASH=REPLACE_WITH_HASH", "token hash placeholder")
	// command: block must NOT be in output.
	assertNotContains(t, out, "command:", "command block")
	// Volume must use /config mount.
	assertContains(t, out, ":/config", "config volume")
	// Migration source annotation.
	assertContains(t, out, "v1.5.1", "migration annotation")
}

func TestMigrateCompose_v160_to_Current(t *testing.T) {
	got, err := MigrateCompose([]byte(composeV160), Schema_v160)
	if err != nil {
		t.Fatalf("MigrateCompose v160→current: %v", err)
	}
	out := string(got)

	assertContains(t, out, "MESH_NAME=node-b", "node name")
	assertContains(t, out, "MESH_MODE=endpoint", "node mode")
	assertContains(t, out, "MESH_TOKEN_HASH=$2b$10$examplehashvalue", "token hash preserved")
	assertContains(t, out, "MESH_CONFIG_DIR=/config", "config dir")
	assertNotContains(t, out, "command:", "command block")
	// restart policy preserved from source.
	assertContains(t, out, "restart: always", "restart policy")
}

func TestMigrateCompose_v190_to_Current(t *testing.T) {
	got, err := MigrateCompose([]byte(composeV190), Schema_v190)
	if err != nil {
		t.Fatalf("MigrateCompose v190→current: %v", err)
	}
	out := string(got)

	assertContains(t, out, "MESH_NAME=node-c", "node name")
	assertContains(t, out, "MESH_CONFIG_DIR=/config", "config dir")
	assertContains(t, out, "MESH_TOKEN_HASH=$2b$10$examplehashvalue", "token hash")
	assertContains(t, out, ":/config", "config volume mount")
}

// ─── MigrateCompose — error paths ─────────────────────────────────────────────

func TestMigrateCompose_UnknownSchema(t *testing.T) {
	_, err := MigrateCompose([]byte(composeCurrent), SchemaUnknown)
	if err == nil {
		t.Fatal("expected error for unknown schema")
	}
}

func TestMigrateCompose_MissingName(t *testing.T) {
	// A v1.9.0-style compose with no MESH_NAME and no --name flag.
	noName := `services:
  awg-mesh-node:
    image: ghcr.io/example/awg-mesh-node:v1.9.0
    network_mode: host
    environment:
      - MESH_TOKEN_HASH=$2b$10$hash
      - MESH_MODE=master
`
	_, err := MigrateCompose([]byte(noName), Schema_v190)
	if err == nil {
		t.Fatal("expected error for missing MESH_NAME")
	}
}

// ─── RestartPolicy preservation ───────────────────────────────────────────────

func TestMigrateCompose_RestartPolicyPreserved(t *testing.T) {
	// v1.6.0 has restart: always — must survive migration.
	got, err := MigrateCompose([]byte(composeV160), Schema_v160)
	if err != nil {
		t.Fatalf("MigrateCompose: %v", err)
	}
	if !strings.Contains(string(got), "restart: always") {
		t.Error("restart policy 'always' was not preserved")
	}
}

// ─── containsEnvKey / extractEnvValue ─────────────────────────────────────────

func TestContainsEnvKey_ListStyle(t *testing.T) {
	content := "    environment:\n      - MESH_TOKEN=abc\n"
	if !containsEnvKey(content, "MESH_TOKEN") {
		t.Error("should detect list-style MESH_TOKEN")
	}
	if containsEnvKey(content, "MESH_TOKEN_HASH") {
		t.Error("should not detect MESH_TOKEN_HASH when only MESH_TOKEN present")
	}
}

func TestContainsEnvKey_MapStyle(t *testing.T) {
	content := "    environment:\n      MESH_NAME: node-x\n"
	if !containsEnvKey(content, "MESH_NAME") {
		t.Error("should detect map-style MESH_NAME")
	}
}

func TestExtractEnvValue_ListStyle(t *testing.T) {
	content := "      - MESH_OVERLAY_IP=10.9.8.7/24\n"
	got := extractEnvValue(content, "MESH_OVERLAY_IP")
	if got != "10.9.8.7/24" {
		t.Errorf("got %q want 10.9.8.7/24", got)
	}
}

func TestExtractEnvValue_MapStyle(t *testing.T) {
	content := "      MESH_LISTEN_PORT: 51820\n"
	got := extractEnvValue(content, "MESH_LISTEN_PORT")
	if got != "51820" {
		t.Errorf("got %q want 51820", got)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func assertContains(t *testing.T, haystack, needle, label string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s: output does not contain %q\nfull output:\n%s", label, needle, haystack)
	}
}

func assertNotContains(t *testing.T, haystack, needle, label string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("%s: output should not contain %q\nfull output:\n%s", label, needle, haystack)
	}
}
