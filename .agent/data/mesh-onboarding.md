# AWG Mesh: Node Onboarding Protocol

**Part of:** mesh-router-design.md
**Created:** 2026-03-26

## Principle: 3-Step Onboarding

```
1. PREPARE  — mesh-ctl generates config + INIT_TOKEN (offline, on admin PC)
2. DEPLOY   — image deployed manually or automatically (docker/mikrotik)
3. INIT     — mesh-ctl connects via token, completes onboarding, switches to mTLS
```

Token is one-time-use. After init completes → token is invalidated, mTLS takes over.

## Step 1: PREPARE

```bash
# MikroTik client:
mesh-ctl client prepare \
  --name mikrotik-home \
  --type mikrotik \
  --masters master-01,master-02

# Linux Docker client:
mesh-ctl client prepare \
  --name office-gateway \
  --type linux \
  --masters master-01,master-02

# Master node (ingress):
mesh-ctl master prepare \
  --name master-03 \
  --type linux

# Endpoint node (egress):
mesh-ctl endpoint prepare \
  --name kz-04 \
  --type linux \
  --region kz
```

### What mesh-ctl does:

```
1. Generate INIT_TOKEN (random, one-time, time-limited)
   e.g., "mesh-init-EXAMPLE"

2. Generate node-specific config:
   For MikroTik: container env vars + .rsc script
     Required params: INIT_TOKEN + CONFIG_DIR (host path for persistent config)
   For Linux: docker-compose.yml + .env
     Required params: INIT_TOKEN + CONFIG_DIR (host volume mount)

3. Save pending node info locally:
   ~/.mesh-ctl/pending/<name>.json
   {
     "name": "mikrotik-home",
     "type": "mikrotik",
     "role": "client",
     "init_token": "<token>",
     "init_token_expires": "2026-03-27T12:00:00Z",
     "masters": ["master-01", "master-02"],
     "status": "pending"
   }

4. Output: config files + instructions
```

### Output for MikroTik:

```
✓ Prepared: mikrotik-home (client, mikrotik)

Files:
  ./mikrotik-home/container-env.txt    # INIT_TOKEN=mesh-init-EXAMPLE
  ./mikrotik-home/setup.rsc            # RouterOS import script

Required:
  1. Create USB directory: /usb1/docker/awg-mikrotik-home/
  2. Set container env: INIT_TOKEN=mesh-init-EXAMPLE
  3. Set container env: CONFIG_DIR=/config (mapped to persistent volume)
  4. Deploy image: ghcr.io/thebtf/awg-agent:latest
  5. Start container
  6. Run: mesh-ctl client init --name mikrotik-home --ip <container-ip>

Token expires: 2026-03-27 12:00 UTC (24h)
```

### Output for Linux Docker:

```
✓ Prepared: office-gateway (client, linux)

Files:
  ./office-gateway/docker-compose.yml
  ./office-gateway/.env                # INIT_TOKEN=mesh-init-EXAMPLE

Required:
  1. Copy to target host
  2. docker compose up -d
  3. Run: mesh-ctl client init --name office-gateway --ip <host-ip>

Token expires: 2026-03-27 12:00 UTC (24h)
```

### docker-compose.yml (generated):

```yaml
services:
  awg-agent:
    image: ghcr.io/thebtf/awg-agent:latest
    container_name: awg-agent
    restart: always
    command: ["--mode", "client", "--init-mode"]
    cap_add: [NET_ADMIN]
    devices: [/dev/net/tun]
    ports:
      - "9090:9090"   # gRPC (init mode: token auth, then mTLS)
    volumes:
      - ${CONFIG_DIR:-./config}:/config   # MUST be persistent
    environment:
      - INIT_TOKEN=${INIT_TOKEN}
```

## Step 2: DEPLOY

Manual or automated — mesh-ctl doesn't care.

- MikroTik: create container in RouterOS UI or CLI
- Linux: `docker compose up -d` on target host
- K8s: apply manifest (future)

awg-agent starts in **init mode**:
- Listens on gRPC port 9090
- Accepts connections authenticated by INIT_TOKEN (not mTLS yet)
- Waits for `Init` RPC call
- Does NOT create tunnels yet

## Step 3: INIT

```bash
# Client:
mesh-ctl client init --name mikrotik-home --ip 172.33.23.28

# Master:
mesh-ctl master init --name master-03 --ip 5.6.7.8

# Endpoint:
mesh-ctl endpoint init --name kz-04 --ip 1.2.3.4
```

### What mesh-ctl does:

```
1. CONNECT to awg-agent at <ip>:9090 with INIT_TOKEN auth

2. EXCHANGE:
   mesh-ctl → agent:
     - mTLS CA certificate
     - Node-specific certificate + private key
     - Node config: role, overlay-ip, masters/endpoints list
     - AWG params (generated from capture data)
     - Peer configs (public keys of counterparts)

   agent → mesh-ctl:
     - Agent's WireGuard public key
     - Agent status confirmation

3. AGENT transitions:
   - Saves mTLS cert to /config/tls/
   - Saves AWG config to /config/wg/
   - Invalidates INIT_TOKEN
   - Restarts gRPC server with mTLS (replaces token auth)
   - Creates WireGuard interfaces
   - Establishes tunnels

4. mesh-ctl:
   - Registers agent's public key on counterpart nodes (via gRPC)
   - Updates topology.yml
   - Moves pending/<name>.json → active/<name>.json
   - git commit

5. VERIFY:
   - Ping overlay IP
   - Check tunnel handshake
   - mesh-ctl status --name <name>
```

### Output:

```
✓ Initialized: mikrotik-home
  Role: client
  Connected to: master-01 (✓ handshake), master-02 (✓ handshake)
  Overlay: 172.20.70.0/24 reachable
  mTLS: active (token invalidated)

  Node is operational. Token can be discarded.
```

## Security Properties

| Phase | Auth method | What can attacker do? |
|-------|------------|----------------------|
| Before prepare | — | Nothing. No token exists. |
| After prepare, before deploy | Has token (in file) | Nothing. No agent listening yet. |
| After deploy, before init | Token in agent env | Connect with stolen token → gets certs. Mitigated: token expires (24h), IP must match. |
| After init | mTLS only | Must have valid cert signed by mesh CA. Token is dead. |
| Operational | mTLS + WG crypto | Must compromise both mTLS cert AND WG private key. |

## Open Questions

(waiting for user input)
