package main

import (
	"strings"
	"testing"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/clientd"
)

func TestClientdCommandValidation(t *testing.T) {
	_, err := clientd.ParseCommandConfig([]string{
		"--control-plane", "127.0.0.1:51820",
		"--name", "client-a",
		"--overlay-ip", "10.10.0.10",
		"--region", "eu-test",
		"--cert", "node.pem",
		"--state-dir", t.TempDir(),
		"--iface", "awg-test0",
		"--protocol", "invalid",
	}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "invalid --protocol") {
		t.Fatalf("expected invalid protocol validation error, got %v", err)
	}
}
