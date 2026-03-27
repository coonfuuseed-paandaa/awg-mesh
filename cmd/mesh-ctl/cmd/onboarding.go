package cmd

import (
	"crypto"
	"crypto/x509"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	pkgtls "github.com/thebtf/awg-mesh/pkg/tls"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

func loadTemplate(name string) (string, error) {
	content, err := templateFS.ReadFile("templates/" + name)
	if err != nil {
		return "", fmt.Errorf("read template %q: %w", name, err)
	}

	return string(content), nil
}

func ensureCA(configDir string) (*x509.Certificate, crypto.PrivateKey, error) {
	caCert, caKey, err := pkgtls.LoadCA(configDir)
	if err == nil {
		return caCert, caKey, nil
	}
	if !os.IsNotExist(err) {
		return nil, nil, err
	}

	caCert, caKey, err = pkgtls.GenerateCA("awg-mesh-ca")
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA: %w", err)
	}

	if err := pkgtls.SaveCA(configDir, caCert, caKey); err != nil {
		return nil, nil, fmt.Errorf("save CA: %w", err)
	}

	return caCert, caKey, nil
}

func saveToken(nodeDir, token string) error {
	if err := os.MkdirAll(nodeDir, 0755); err != nil {
		return fmt.Errorf("create node dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "token"), []byte(token), 0600); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	return nil
}

func loadToken(nodeDir string) (string, error) {
	rawToken, err := os.ReadFile(filepath.Join(nodeDir, "token"))
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	return strings.TrimSpace(string(rawToken)), nil
}

func containsName(list []string, needle string) bool {
	for _, value := range list {
		if value == needle {
			return true
		}
	}
	return false
}

func renderDockerCompose(tmplContent string, data any, outputPath string) error {
	tmpl, err := template.New("compose").Parse(tmplContent)
	if err != nil {
		return fmt.Errorf("parse compose template: %w", err)
	}

	var output strings.Builder
	if err := tmpl.Execute(&output, data); err != nil {
		return fmt.Errorf("render compose template: %w", err)
	}

	if err := os.WriteFile(outputPath, []byte(output.String()), 0644); err != nil {
		return fmt.Errorf("write compose file: %w", err)
	}
	return nil
}

func nodeDir(configDir, name string) string {
	return filepath.Join(configDir, "nodes", name)
}

func caPath(configDir string) string {
	return filepath.Join(configDir, "ca.crt")
}
