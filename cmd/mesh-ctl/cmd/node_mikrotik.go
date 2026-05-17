package cmd

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/awgmesh"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/mikrotik"
	mikrotikv2 "github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/mikrotik/v2"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/role"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/topology"
	"github.com/coonfuuseed-paandaa/awg-mesh/v2/pkg/wg"
)

const (
	routerOSScriptFilename       = "routeros.rsc"
	mikrotikClientImageRepo      = "ghcr.io/coonfuuseed-paandaa/awg-mesh-client"
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

func defaultMikrotikClientImage() string {
	tag := strings.TrimSpace(awgmesh.Version)
	if tag == "" || tag == "dev" {
		tag = "latest"
	}
	return mikrotikClientImageRepo + ":" + tag
}

func prepareMikrotikRouterOS(topo *topology.TopologyV2, node topology.NodeV2, configDir, nodeDir, tokenHash, controlPlane, targetROS string) (mikrotikPrepareArtifacts, error) {
	if !nodeHasRole(node, role.RoleClient) {
		return mikrotikPrepareArtifacts{}, fmt.Errorf("node %q must have role client for --platform mikrotik", node.Name)
	}
	if strings.TrimSpace(controlPlane) == "" {
		return mikrotikPrepareArtifacts{}, fmt.Errorf("--control-plane is required as the responsible master coordination target for --platform mikrotik")
	}
	certB64, err := readBase64File(filepath.Join(nodeDir, "node.crt"))
	if err != nil {
		return mikrotikPrepareArtifacts{}, fmt.Errorf("read node certificate for %q: %w", node.Name, err)
	}
	keyB64, err := readBase64File(filepath.Join(nodeDir, "node.key"))
	if err != nil {
		return mikrotikPrepareArtifacts{}, fmt.Errorf("read node key for %q: %w", node.Name, err)
	}
	caB64, err := readBase64File(filepath.Join(configDir, "ca.crt"))
	if err != nil {
		return mikrotikPrepareArtifacts{}, fmt.Errorf("read mesh CA certificate: %w", err)
	}

	containerName := mikrotik.DeriveContainerName(node.Name)
	endpointHost, err := nodeAdvertisedMeshEndpoint(node)
	if err != nil {
		return mikrotikPrepareArtifacts{}, err
	}
	script, err := mikrotik.GenerateDeployRSC(mikrotik.DeployScript{
		TopologyName:  node.Name,
		ContainerName: containerName,
		Image:         defaultMikrotikClientImage(),
		Veth:          containerName,
		OverlayIP:     node.OverlayIP,
		OverlayNet:    topo.Mesh.OverlaySupernet,
		TokenHash:     tokenHash,
		NodeCertB64:   certB64,
		NodeKeyB64:    keyB64,
		CACertB64:     caB64,
		ControlPlane:  controlPlane,
		Region:        node.Region,
		ListenPort:    nodeMeshListenPort(node),
		EndpointHost:  endpointHost,
		TargetROS:     targetROS,
	})
	if err != nil {
		return mikrotikPrepareArtifacts{}, fmt.Errorf("generate mikrotik RouterOS container script for %q: %w", node.Name, err)
	}
	scriptPath := filepath.Join(nodeDir, routerOSScriptFilename)
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return mikrotikPrepareArtifacts{}, fmt.Errorf("write RouterOS script for %q: %w", node.Name, err)
	}

	return mikrotikPrepareArtifacts{
		RouterOSScriptPath: scriptPath,
	}, nil
}

func readBase64File(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func prepareMikrotikNativeWireGuardRouterOS(topo *topology.TopologyV2, node topology.NodeV2, configDir, nodeDir string) (mikrotikPrepareArtifacts, error) {
	if !nodeHasRole(node, role.RoleClient) {
		return mikrotikPrepareArtifacts{}, fmt.Errorf("node %q must have role client for native RouterOS WireGuard", node.Name)
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
	key, err := readNonZeroWGKey(path, "private")
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return wg.Key{}, err
	}

	generated, err := wg.GeneratePrivateKey()
	if err != nil {
		return wg.Key{}, fmt.Errorf("generate private key: %w", err)
	}

	if err := writeFileExclusive(path, []byte(generated.String()+"\n"), 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readNonZeroWGKey(path, "private")
		}
		return wg.Key{}, fmt.Errorf("write private key %s: %w", path, err)
	}

	stored, err := readNonZeroWGKey(path, "private")
	if err != nil {
		return wg.Key{}, err
	}
	if stored != generated {
		return wg.Key{}, fmt.Errorf("private key %s changed during atomic create", path)
	}
	return stored, nil
}

func syncPublicWGKey(path string, expected wg.Key) error {
	if err := requireStoredPublicWGKey(path, expected); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := writeFileExclusive(path, []byte(expected.String()+"\n"), 0o644); err != nil {
		if errors.Is(err, os.ErrExist) {
			return requireStoredPublicWGKey(path, expected)
		}
		return fmt.Errorf("write public key %s: %w", path, err)
	}
	return requireStoredPublicWGKey(path, expected)
}

func requireStoredPublicWGKey(path string, expected wg.Key) error {
	existing, err := readNonZeroWGKey(path, "public")
	if err != nil {
		return err
	}
	if existing != expected {
		return fmt.Errorf("public key %s does not match private key", path)
	}
	return nil
}

func readNonZeroWGKey(path, kind string) (wg.Key, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		key, parseErr := wg.ParseKey(strings.TrimSpace(string(raw)))
		if parseErr != nil {
			return wg.Key{}, fmt.Errorf("parse %s key %s: %w", kind, path, parseErr)
		}
		if key.IsZero() {
			return wg.Key{}, fmt.Errorf("%s key %s must not be zero", kind, path)
		}
		return key, nil
	}
	return wg.Key{}, fmt.Errorf("read %s key %s: %w", kind, path, err)
}

func writeFileExclusive(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()

	defer func() {
		_ = os.Remove(tempPath)
	}()

	n, err := file.Write(data)
	if err != nil {
		_ = file.Close()
		return err
	}
	if n != len(data) {
		_ = file.Close()
		return io.ErrShortWrite
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(tempPath, path); err != nil {
		return err
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
