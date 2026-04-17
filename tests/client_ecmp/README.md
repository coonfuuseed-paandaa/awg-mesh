# Client ECMP — Manual End-to-End Test Fixture

## Purpose

This fixture provides manual end-to-end verification for two user stories from
`.agent/specs/client-ecmp/spec.md`:

- **US1 — Master failover:** when one master node goes down, the client ECMP route
  converges to the surviving master within the healthcheck window. Traffic continues
  uninterrupted. When the failed master recovers, the client re-adds its nexthop.
- **US2 — Session stickiness:** ECMP routing distributes new flows across both masters
  while keeping existing flows pinned to their original nexthop for the lifetime of the
  session.

This fixture is **not run in CI**. Operators run it manually on a Linux development host
or WSL2 environment where privileged Docker containers are available.

---

## Prerequisites

| Requirement | Notes |
|---|---|
| Linux host or WSL2 | macOS Docker Desktop does not support `NET_ADMIN` inside containers reliably |
| Docker Engine 24+ | `docker --version` must report 24.x or newer |
| Docker Compose v2 | `docker compose version` (note: no hyphen) must report v2.x |
| ~500 MB free disk | Images are built locally; no registry pull required |
| Kernel with `TUN` module | `modinfo tun` must succeed; enabled by default on all modern distributions |
| Kernel with `nftables` | `nft --version` must succeed; enabled by default since kernel 5.2 |
| `CAP_NET_ADMIN` permitted | Default on most distros; blocked on hardened container runtimes |

---

## Quick Start

```bash
# 1. Build images and start all containers
docker compose -f tests/client_ecmp/compose.yml up -d --build

# 2. Run automated verification (US1 + US2)
bash tests/client_ecmp/verify.sh

# 3. Tear down and remove volumes
docker compose -f tests/client_ecmp/compose.yml down -v
```

---

## Manual Test Procedure

### US1 — Master Failover

**Goal:** verify client route converges after master-01 goes down, then recovers when it comes back.

**Step 1: Confirm ECMP route with both masters present**

```bash
docker exec client-lin ip route show 172.20.70.0/24
```

Expected output (multipath route, both nexthops):
```
172.20.70.0/24
    nexthop via 172.31.0.10 dev eth0 weight 1
    nexthop via 172.31.0.11 dev eth0 weight 1
```

**Step 2: Kill master-01 and wait for failover**

```bash
docker kill master-01
sleep 35
docker exec client-lin ip route show 172.20.70.0/24
```

Expected output (only master-02 nexthop remains):
```
172.20.70.0/24 via 172.31.0.11 dev eth0
```

**Step 3: Restart master-01 and wait for recovery**

```bash
docker start master-01
sleep 60
docker exec client-lin ip route show 172.20.70.0/24
```

Expected output (both nexthops restored):
```
172.20.70.0/24
    nexthop via 172.31.0.10 dev eth0 weight 1
    nexthop via 172.31.0.11 dev eth0 weight 1
```

---

### US2 — Session Stickiness

**Goal:** verify new flows are distributed across masters while established flows remain
on their original nexthop.

**Step 1: Confirm ECMP route**

```bash
docker exec client-lin ip route show 172.20.70.0/24
```

Both nexthops (172.31.0.10 and 172.31.0.11) must appear.

**Step 2: Inspect existing sessions (optional, requires conntrack on host)**

```bash
# If conntrack is available on the host:
conntrack -L | grep udp
```

Each established flow shows the nexthop it was pinned to. New flows hash to either master
based on the 5-tuple; existing entries are not migrated until they expire.

**Step 3: Verify overlay reachability from client**

```bash
docker exec client-lin ping -c 3 172.20.70.2   # master-01 overlay IP
docker exec client-lin ping -c 3 172.20.70.3   # master-02 overlay IP
docker exec client-lin ping -c 3 172.20.70.37  # node-eu-01 overlay IP
```

All three pings must succeed (packet loss 0%).

---

## Cleanup

```bash
# Stop containers and remove volumes
docker compose -f tests/client_ecmp/compose.yml down -v

# Remove runtime state directories (AWG keys, configs)
rm -rf tests/client_ecmp/compose-state/
```

---

## Troubleshooting

| Symptom | Likely cause | Remedy |
|---|---|---|
| Container exits immediately with "open /dev/net/tun: no such file or directory" | TUN kernel module not loaded | `sudo modprobe tun` on the host, then retry |
| Container exits with "operation not permitted" during nftables setup | `NET_ADMIN` capability denied by Docker or seccomp profile | Check Docker daemon seccomp profile; use `--security-opt seccomp=unconfined` for testing only |
| `docker compose up` fails with "network awg-mesh-test already exists" | Previous run left the network behind | `docker compose -f tests/client_ecmp/compose.yml down` or `docker network rm awg-mesh-test` |
| `docker compose up` fails with "port is already allocated" (51820-51822) | Another container or AWG instance holds the port | Stop conflicting service: `sudo ss -ulpn \| grep 5182` |
| Images fail to build (golang:1.25-alpine not found) | Docker Hub rate limit or network unreachable | `docker login` to authenticate, or wait and retry |
| `verify.sh: permission denied` | Script not executable | `chmod +x tests/client_ecmp/verify.sh` |
| `docker compose` not found (only `docker-compose`) | Docker Compose v1 installed instead of v2 | Install Docker Compose plugin: `docker compose version` must report v2.x |
| Client route shows only one nexthop immediately after `up` | Convergence still in progress | Wait 10-15s; healthcheck probes must complete before both paths appear |

---

## Limitations

- This fixture requires **privileged Docker on Linux** and does not run in CI. The GitHub
  Actions workflow only runs unit tests and the standard `go test ./...` suite.
- Operators must execute the fixture manually on a Linux development host or WSL2.
- The compose network (`172.31.0.0/24`) and overlay range (`172.20.70.0/24`) are
  hardcoded for local testing; they must not conflict with host routing tables.
- Token hashes in `compose.yml` are placeholder values. In production, use
  `mesh-ctl token generate` to produce real bcrypt hashes.
- For authoritative spec details on US1, US2, acceptance criteria, and ECMP
  implementation, see `.agent/specs/client-ecmp/spec.md`.
