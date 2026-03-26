# Integration Tests

Two-node Docker Compose integration tests for `awg-mesh-node`. These tests verify
container startup, keypair operations, and (when AWG interface creation is wired)
full overlay tunnel connectivity.

## Prerequisites

- Docker Desktop with Linux containers enabled
- Go 1.25+

## Build the test image

From the project root:

```sh
docker build -t awg-mesh:test -f deploy/Dockerfile .
```

`TestMain` also builds the image automatically before running any tests, so this
step is optional when running via `go test`.

## Run all integration tests

From the project root:

```sh
go test -tags integration -v ./tests/integration/
```

## Run a specific test

```sh
go test -tags integration -v -run TestKeypairGeneration ./tests/integration/
go test -tags integration -v -run TestContainerStartup   ./tests/integration/
```

## Tests

### TestContainerStartup

Starts both `node-server` and `node-client` containers via `docker compose up -d`
and polls `docker compose ps` until both containers appear (up to 30 s). Verifies
that the binary accepts its flags and runs inside the Alpine Docker image.

Cleans up with `docker compose down -v` on exit.

### TestKeypairGeneration

Pure unit test — no Docker required. Verifies that:

- `wg.GeneratePrivateKey()` produces a non-zero key.
- `Key.PublicKey()` derives a non-zero public key.
- Keys encode to 44-character standard base64 strings.
- `wg.ParseKey(key.String())` round-trips correctly.

### TestTwoNodeTunnel (skipped)

Currently skipped with `t.Skip(...)`. Will be enabled once `endpoint.go` and
`client.go` wire up actual TUN device creation and AWG peer configuration.

When enabled, the test will:

1. Generate keypairs for both nodes.
2. Write AWG config files into temp directories mounted as volumes.
3. Start `docker compose up -d`.
4. Poll `docker exec node-server wg show` until a handshake is recorded.
5. Run `docker exec node-client ping -c 3 172.20.70.2` and assert exit code 0.

## Docker Compose topology

| Container   | Mode     | Overlay IP   | Host IP (test-mesh)  |
|-------------|----------|--------------|----------------------|
| node-server | endpoint | 172.20.70.2  | 192.168.100.10       |
| node-client | client   | 172.20.70.3  | 192.168.100.11       |

Both containers require `NET_ADMIN` and `NET_RAW` capabilities and access to
`/dev/net/tun` for WireGuard TUN device creation.
