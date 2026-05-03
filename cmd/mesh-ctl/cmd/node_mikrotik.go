package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	mikrotikv2 "github.com/coonfuuseed-paandaa/awg-mesh/pkg/mikrotik/v2"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/pkg/wg"
)

const (
	routerOSScriptFilename       = "routeros.rsc"
	mikrotikWGPrivateKeyFile     = "wireguard-private.key"
	mikrotikWGPublicKeyFile      = "wireguard-public.key"
	masterClientWGPrivateKeyFile = "client-wg-private.key"
	masterClientWGPublicKeyFile  = "client-wg-public.key"
)

type mikrotikPrepareArtifacts struct {
	RouterOSScriptPath      string
	WireGuardPrivateKeyPath string
	WireGuardPublicKeyPath  string
}

func preparePlatform(override string, node topology.NodeV2) string {
	if trimmed := strings.ToLower(strings.TrimSpace(override)); trimmed != "" {
		return trimmed
	}
	if strings.EqualFold(strings.TrimSpace(node.Platform), "mikrotik") {
		return "mikrotik"
	}
	return ""
}

func prepareMikrotikRouterOS(topo *topology.TopologyV2, node topology.NodeV2, configDir, nodeDir string) (mikrotikPrepareArtifacts, error) {
	if !nodeHasRole(node, role.RoleClient) {
		return mikrotikPrepareArtifacts{}, fmt.Errorf("node %q must have role client for --platform mikrotik", node.Name)
	}
	clientPrivate, _, clientPaths, err := loadOrCreateWireGuardKeyPair(nodeDir, mikrotikWGPrivateKeyFile, mikrotikWGPublicKeyFile)
	if err != nil {
		return mikrotikPrepareArtifacts{}, fmt.Errorf("prepare mikrotik client keypair for %q: %w", node.Name, err)
	}

	masterPublicKeys := make(map[string]wg.Key)
	for _, current := range topo.Nodes {
		if !nodeHasRole(current, role.RoleMaster) {
			continue
		}
		masterDir, err := safeNodeConfigDir(configDir, current.Name)
		if err != nil {
			return mikrotikPrepareArtifacts{}, err
		}
		_, masterPublic, _, err := loadOrCreateWireGuardKeyPair(masterDir, masterClientWGPrivateKeyFile, masterClientWGPublicKeyFile)
		if err != nil {
			return mikrotikPrepareArtifacts{}, fmt.Errorf("prepare master client-facing keypair for %q: %w", current.Name, err)
		}
		masterPublicKeys[current.Name] = masterPublic
	}

	script, err := mikrotikv2.GenerateStaticRSC(topo, node.Name, clientPrivate, masterPublicKeys)
	if err != nil {
		return mikrotikPrepareArtifacts{}, fmt.Errorf("generate mikrotik RouterOS script for %q: %w", node.Name, err)
	}
	scriptPath := filepath.Join(nodeDir, routerOSScriptFilename)
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return mikrotikPrepareArtifacts{}, fmt.Errorf("write RouterOS script for %q: %w", node.Name, err)
	}

	return mikrotikPrepareArtifacts{
		RouterOSScriptPath:      scriptPath,
		WireGuardPrivateKeyPath: clientPaths.PrivateKeyPath,
		WireGuardPublicKeyPath:  clientPaths.PublicKeyPath,
	}, nil
}

type wireGuardKeyPairPaths struct {
	PrivateKeyPath string
	PublicKeyPath  string
}

func loadOrCreateWireGuardKeyPair(dir, privateFilename, publicFilename string) (wg.Key, wg.Key, wireGuardKeyPairPaths, error) {
	paths := wireGuardKeyPairPaths{
		PrivateKeyPath: filepath.Join(dir, privateFilename),
		PublicKeyPath:  filepath.Join(dir, publicFilename),
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return wg.Key{}, wg.Key{}, paths, fmt.Errorf("create key directory: %w", err)
	}

	privateKey, err := loadOrCreatePrivateWGKey(paths.PrivateKeyPath)
	if err != nil {
		return wg.Key{}, wg.Key{}, paths, err
	}
	publicKey := privateKey.PublicKey()
	if err := syncPublicWGKey(paths.PublicKeyPath, publicKey); err != nil {
		return wg.Key{}, wg.Key{}, paths, err
	}
	return privateKey, publicKey, paths, nil
}

func loadOrCreatePrivateWGKey(path string) (wg.Key, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		key, parseErr := wg.ParseKey(strings.TrimSpace(string(raw)))
		if parseErr != nil {
			return wg.Key{}, fmt.Errorf("parse private key %s: %w", path, parseErr)
		}
		if key.IsZero() {
			return wg.Key{}, fmt.Errorf("private key %s must not be zero", path)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return wg.Key{}, fmt.Errorf("read private key %s: %w", path, err)
	}

	key, err := wg.GeneratePrivateKey()
	if err != nil {
		return wg.Key{}, fmt.Errorf("generate private key: %w", err)
	}
	if err := os.WriteFile(path, []byte(key.String()+"\n"), 0o600); err != nil {
		return wg.Key{}, fmt.Errorf("write private key %s: %w", path, err)
	}
	return key, nil
}

func syncPublicWGKey(path string, expected wg.Key) error {
	raw, err := os.ReadFile(path)
	if err == nil {
		existing, parseErr := wg.ParseKey(strings.TrimSpace(string(raw)))
		if parseErr != nil {
			return fmt.Errorf("parse public key %s: %w", path, parseErr)
		}
		if existing != expected {
			return fmt.Errorf("public key %s does not match private key", path)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("read public key %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(expected.String()+"\n"), 0o644); err != nil {
		return fmt.Errorf("write public key %s: %w", path, err)
	}
	return nil
}

func nodeHasRole(node topology.NodeV2, want role.Role) bool {
	for _, current := range node.Roles {
		if current == want {
			return true
		}
	}
	return false
}
