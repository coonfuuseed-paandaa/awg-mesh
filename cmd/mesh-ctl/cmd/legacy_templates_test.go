package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyComposeTemplatesAreRemoved(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "templates", "docker-compose.master.yml.tmpl"),
		filepath.Join("..", "templates", "docker-compose.endpoint.yml.tmpl"),
		filepath.Join("..", "templates", "docker-compose.client.yml.tmpl"),
		filepath.Join("..", "..", "..", "deploy", "templates", "docker-compose.master.yml.tmpl"),
		filepath.Join("..", "..", "..", "deploy", "templates", "docker-compose.endpoint.yml.tmpl"),
		filepath.Join("..", "..", "..", "deploy", "templates", "docker-compose.client.yml.tmpl"),
	} {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("legacy v1 compose template still exists: %s", path)
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", path, err)
		}
	}
}

func TestBootstrapHelpPointsToNodePrepare(t *testing.T) {
	cmd := newBootstrapCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("bootstrap help: %v", err)
	}
	help := out.String()
	if !strings.Contains(help, "mesh-ctl node prepare") {
		t.Fatalf("bootstrap help does not mention node prepare:\n%s", help)
	}
	for _, legacy := range []string{"mesh-ctl master prepare", "mesh-ctl endpoint prepare", "mesh-ctl client prepare"} {
		if strings.Contains(help, legacy) {
			t.Fatalf("bootstrap help still mentions legacy command %q:\n%s", legacy, help)
		}
	}
}
