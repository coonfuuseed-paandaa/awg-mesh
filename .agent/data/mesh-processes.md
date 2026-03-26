# AWG Mesh: Operational Processes

**Part of:** mesh-router-design.md
**Created:** 2026-03-26
**Updated:** 2026-03-26 (simplified onboarding: prepare → deploy → init)

## Components

| Component | Where | Image | What |
|-----------|-------|-------|------|
| **mesh-ctl** | Admin PC | — (binary) | Control plane CLI. All management. |
| **mesh-router** | Master node | ghcr.io/thebtf/mesh-router | Data plane. Labels → routes, healthcheck. |
| **awg-agent** | Everywhere | ghcr.io/thebtf/awg-agent | Unified AWG node. Modes: server/client/ingress/config-only. |

awg-agent replaces awg-node, awg-client, awg-ingress — single binary, different `--mode`.

## Universal Onboarding: prepare → deploy → init

All node types (client, master, endpoint) follow the same 3-step protocol:

```
1. mesh-ctl <role> prepare   → generates config + INIT_TOKEN
2. Image deployed            → manually or automated
3. mesh-ctl <role> init      → connects via token, completes setup, switches to mTLS
```

See mesh-onboarding.md for full protocol detail.

---

## 1. Add Endpoint Node (egress)

### Prepare

```bash
mesh-ctl endpoint prepare \
  --name kz-04 \
  --region kz \
  --port 853 \
  --overlay-ip 172.20.70.24 \
  --balancer-ip 172.20.70.20
```

Output: `./kz-04/docker-compose.yml` + `./kz-04/.env` (with INIT_TOKEN, CONFIG_DIR)

### Deploy

```bash
# Copy to endpoint host, then:
docker compose up -d
# awg-agent starts in --mode server --init-mode, waits for init
```

### Init

```bash
mesh-ctl endpoint init --name kz-04 --ip 1.2.3.4
```

What happens:
1. mesh-ctl connects to awg-agent:9090 with INIT_TOKEN
2. Sends: mTLS certs, overlay-ip config, NAT rules
3. awg-agent saves config, starts AWG server on port 853
4. Token invalidated → mTLS active
5. mesh-ctl registers kz-04 on all masters:
   - For each master: generates unique keypair + AWG params
   - Calls master's awg-agent (gRPC mTLS): creates awg-client container
   - mesh-router auto-discovers → adds route + balancer pool
6. Updates topology.yml, commits

### Output:
```
✓ Endpoint kz-04 initialized at 1.2.3.4:853
  Overlay: 172.20.70.24
  Balancer: 172.20.70.20 (pool: kz-01, kz-02, kz-03, kz-04)
  Registered on: master-01 ✓, master-02 ✓
```

### Remove

```bash
mesh-ctl endpoint remove --name kz-04
```

1. For each master: stops awg-client-kz-04, removes peer
2. mesh-router auto-removes route + balancer entry
3. On endpoint: stops awg-agent (optional: --keep-host)
4. Updates topology, commits

---

## 2. Add Master Node (ingress)

### Prepare

```bash
mesh-ctl master prepare \
  --name master-03 \
  --overlay-ip 172.20.70.13
```

Output: `./master-03/docker-compose.yml` + `.env` (INIT_TOKEN, CONFIG_DIR)

docker-compose includes: mesh-router + awg-agent (ingress mode)

### Deploy

```bash
# Copy to master host, then:
docker compose up -d
# mesh-router + awg-agent start in init mode
```

### Init

```bash
mesh-ctl master init --name master-03 --ip 5.6.7.8
```

What happens:
1. mesh-ctl connects with INIT_TOKEN → sends mTLS certs
2. awg-agent switches to mTLS, starts awg-ingress (server for MikroTik)
3. mesh-ctl creates awg-client containers for EACH endpoint in topology:
   - Generates unique keypair + AWG params per tunnel
   - Registers peer on each endpoint's awg-agent (gRPC mTLS)
   - Deploys awg-client containers with mesh labels
4. mesh-router discovers labels → builds routes
5. Sets up capture data cron (daily TLS/QUIC capture)
6. Updates topology, commits

### Then on MikroTik (separate step):

```bash
mesh-ctl client prepare --name mikrotik-master-03 --type mikrotik --masters master-03
# → generates .conf + RouterOS commands
# → manual deploy on MikroTik
mesh-ctl client init --name mikrotik-master-03 --ip 172.33.23.XX
# → registers MikroTik peer on master-03 ingress
# → adds ECMP route to MikroTik
```

### Remove

```bash
mesh-ctl master remove --name master-03
```

1. MikroTik: remove AWG peer + ECMP route (generates .rsc script)
2. On each endpoint: removes master-03's peer
3. On master-03: stops all containers
4. Updates topology, commits

---

## 3. Add Client (MikroTik or Linux)

### Prepare

```bash
# MikroTik:
mesh-ctl client prepare --name mikrotik-home --type mikrotik --masters master-01,master-02

# Linux Docker:
mesh-ctl client prepare --name office-gw --type linux --masters master-01,master-02
```

Output:
- MikroTik: container env + .rsc script (INIT_TOKEN + CONFIG_DIR)
- Linux: docker-compose.yml + .env (INIT_TOKEN + CONFIG_DIR)

### Deploy

Manual: create container on MikroTik or `docker compose up -d` on Linux.

### Init

```bash
mesh-ctl client init --name mikrotik-home --ip 172.33.23.28
```

1. Connects with INIT_TOKEN
2. Sends AWG configs for each master
3. Agent creates tunnels, switches to mTLS (Linux) or config-only (MikroTik)
4. Registers client peer on each master's ingress
5. Verifies tunnels

### Remove

```bash
mesh-ctl client remove --name mikrotik-home
```

---

## 4. Key & AWG Parameter Rotation

### Tiers

| Tier | What | Scope | Downtime |
|------|------|-------|----------|
| 1 | Jc, Jmin, Jmax, I1-I5 | Per-tunnel | Zero (UAPI) |
| 2 | S1-S4, H1-H4 | Per-endpoint server | Zero (UAPI, coordinated) |
| 3 | WireGuard keypair | Per-tunnel | Brief (~2s reconnect) |

### Commands

```bash
# Rotate specific tunnel:
mesh-ctl rotate --tier 1 --from master-01 --to kz-01

# Rotate all tunnels on a master:
mesh-ctl rotate --tier 1 --master master-01

# Rotate all tunnels to an endpoint:
mesh-ctl rotate --tier 1 --endpoint kz-01

# Rotate everything:
mesh-ctl rotate --tier 1

# Rotate server params (coordinated across all masters):
mesh-ctl rotate --tier 2 --endpoint kz-01

# Rotate keypair:
mesh-ctl rotate --tier 3 --from master-01 --to kz-01

# Run scheduled rotations (checks topology intervals):
mesh-ctl rotate --scheduled

# Rotate MikroTik client tunnels (sequential, ECMP covers):
mesh-ctl rotate --client mikrotik-home --tier 1
```

### Tier 1 Protocol (per-tunnel, zero downtime)

```
1. Generate new J/I params (from master's capture data)
2. Apply to endpoint awg-agent peer (gRPC → UAPI SET)
3. Apply to master awg-agent client (gRPC → UAPI SET)
   (steps 2+3 within seconds — AWG tolerates brief mismatch)
4. Verify: ping overlay IP
5. If fail → rollback both sides
6. Persist new config to disk
```

### Tier 2 Protocol (per-endpoint server params, coordinated)

```
1. Preflight: check ALL masters connected to this endpoint are healthy
   If any unreachable → abort (would break reconnection)
2. Generate new S/H params
3. For each master (sequentially):
   a. Apply to endpoint peer config (UAPI SET)
   b. Apply to master client config (UAPI SET)
   c. Verify tunnel
4. Persist
```

### Tier 3 Protocol (keypair rotation)

```
1. Generate new keypair on client side
2. Register new public key as ADDITIONAL peer on server
   (server accepts both old and new key)
3. Apply new private key to client (UAPI or restart)
4. Verify tunnel with new key
5. Remove old peer from server
6. Persist
```

### MikroTik Client Rotation

```
1. Preflight: ≥2 masters healthy (ECMP redundancy)
2. For each master (sequentially):
   a. Generate new params
   b. Apply to master ingress (gRPC → UAPI, zero downtime)
   c. Write new .conf to SMB share (R:\docker_conf\...)
   d. Restart MikroTik container (~2-5s downtime, ECMP covers)
   e. Verify handshake
   f. Wait 10s before next master
```

### Configurable Schedule (per-endpoint)

```yaml
# In topology.yml:
endpoints:
  - name: kz-01
    rotation:
      tier1_interval: 24h
      tier2_interval: 7d
      tier3_interval: 30d
      randomize_window: 2h
      preset: aggressive
```

---

## 5. AWG Parameter Generation

### Engine: Go port of amneziawg-scripts

Core logic from `awg_gen.py` ported to `pkg/awggen/`:
- 9 protocol families (TLS, QUIC, DNS, STUN, DTLS, NTP, HTTP, WebSocket, TURN)
- 3 presets (aggressive, balanced, minimal)
- I-spec tag encoding (<b>, <r>, <rc>, <rd>, <t> — NO <c>)
- MTU constraint validation
- Capture data parsing

```bash
# Standalone generation (for debugging):
mesh-ctl generate --preset aggressive --count 3
mesh-ctl generate --filter quic --output json
```

---

## 6. Daily Capture Data Refresh

```bash
# Refresh specific master:
mesh-ctl capture refresh --master master-01

# Refresh all masters:
mesh-ctl capture refresh

# Set up schedule:
mesh-ctl capture schedule --interval 24h --window 02:00-05:00

# Manage domain list:
mesh-ctl capture domains --list
mesh-ctl capture domains --add example.com
mesh-ctl capture domains --import domains.txt
```

### What happens:

```
1. mesh-ctl calls awg-agent on master (gRPC):
   → CaptureRefresh(domains=["yandex.ru", "vk.com", ...])
2. awg-agent connects to each domain:
   → TLS ClientHello → save .bin
   → QUIC Initial → save .bin
3. Stores in /config/capture-data/ (persistent volume)
4. Prunes old captures (retention: 7 days)
5. Next rotation uses fresh capture data
```

Each master captures independently → different packet data → different generated params → different DPI fingerprints per master.

---

## 7. Communication: gRPC with mTLS

### All management via gRPC (SSH only for initial host setup)

```protobuf
service AwgAgent {
  // Init (token auth, one-time)
  rpc Init(InitRequest) returns (InitResponse);

  // Peer management (mTLS)
  rpc AddPeer(AddPeerRequest) returns (AddPeerResponse);
  rpc RemovePeer(RemovePeerRequest) returns (RemovePeerResponse);
  rpc ListPeers(Empty) returns (PeerList);

  // Rotation (mTLS)
  rpc RotateParams(RotateParamsRequest) returns (RotateParamsResponse);
  rpc GetParams(GetParamsRequest) returns (AwgParams);

  // Capture (mTLS, master nodes only)
  rpc CaptureRefresh(CaptureRequest) returns (CaptureResponse);

  // Status
  rpc GetStatus(Empty) returns (NodeStatus);
  rpc HealthCheck(Empty) returns (HealthResponse);
}

service MeshRouter {
  rpc GetRoutes(Empty) returns (RouteTable);
  rpc GetPeers(Empty) returns (PeerStatusList);
  rpc GetHealth(Empty) returns (HealthResponse);
}
```

### Bootstrap (SSH, one-time)

Only for installing Docker on a bare host. mesh-ctl can optionally do this:
```bash
mesh-ctl bootstrap --host 1.2.3.4 --ssh-key ~/.ssh/id_ed25519
# Installs Docker, pulls images. Then: prepare → deploy → init as usual.
```

---

## Command Reference

```bash
# Prepare (generates config + token)
mesh-ctl endpoint prepare --name X --region R --overlay-ip IP
mesh-ctl master prepare --name X --overlay-ip IP
mesh-ctl client prepare --name X --type mikrotik|linux --masters M1,M2

# Init (connects via token, completes onboarding)
mesh-ctl endpoint init --name X --ip IP
mesh-ctl master init --name X --ip IP
mesh-ctl client init --name X --ip IP

# Remove
mesh-ctl endpoint remove --name X
mesh-ctl master remove --name X
mesh-ctl client remove --name X

# Status
mesh-ctl status                                # full mesh overview
mesh-ctl status --master X                     # master detail
mesh-ctl status --endpoint X                   # endpoint detail

# Rotation
mesh-ctl rotate --tier 1|2|3                   # rotate all
mesh-ctl rotate --tier 1 --master X --endpoint Y
mesh-ctl rotate --client X --tier 1
mesh-ctl rotate --scheduled                    # check intervals, rotate due

# Capture
mesh-ctl capture refresh [--master X]
mesh-ctl capture schedule --interval 24h
mesh-ctl capture domains --list|--add|--import

# Generation (standalone)
mesh-ctl generate --preset aggressive --count N

# Bootstrap (optional, SSH)
mesh-ctl bootstrap --host IP

# Debug
mesh-ctl ping OVERLAY_IP --from MASTER
mesh-ctl logs MASTER CONTAINER
mesh-ctl routes MASTER
```
