# Mesh Simulation

Local Docker simulation of a production-like mesh: 2 masters, 5 endpoints.

## Prerequisites

- Docker Desktop running
- `awg-mesh-node:transport` image built: `docker build -f deploy/Dockerfile -t awg-mesh-node:transport .`
- `mesh-ctl` installed: `go install ./cmd/mesh-ctl`

## Start

```bash
docker compose -f tests/simulation/docker-compose.yml up -d
```

All 7 containers start with gRPC listening + AWG interfaces + overlay IPs on loopback.

## Initialize mesh

```bash
# Prepare all nodes (generates tokens + docker-compose snippets)
mesh-ctl -t tests/simulation/mesh-topology.yml endpoint prepare --name kz-01
mesh-ctl -t tests/simulation/mesh-topology.yml endpoint prepare --name kz-02
mesh-ctl -t tests/simulation/mesh-topology.yml endpoint prepare --name kz-03
mesh-ctl -t tests/simulation/mesh-topology.yml endpoint prepare --name pl-01
mesh-ctl -t tests/simulation/mesh-topology.yml endpoint prepare --name us-01
mesh-ctl -t tests/simulation/mesh-topology.yml master prepare --name ru-01
mesh-ctl -t tests/simulation/mesh-topology.yml master prepare --name ru-02

# Initialize endpoints (exchange certs, allocate transport, set up tunnels)
mesh-ctl -t tests/simulation/mesh-topology.yml endpoint init --name kz-01
mesh-ctl -t tests/simulation/mesh-topology.yml endpoint init --name kz-02
mesh-ctl -t tests/simulation/mesh-topology.yml endpoint init --name kz-03
mesh-ctl -t tests/simulation/mesh-topology.yml endpoint init --name pl-01
mesh-ctl -t tests/simulation/mesh-topology.yml endpoint init --name us-01

# Initialize masters (exchange certs, create tunnels to all endpoints)
mesh-ctl -t tests/simulation/mesh-topology.yml master init --name ru-01
mesh-ctl -t tests/simulation/mesh-topology.yml master init --name ru-02
```

## Verify

```bash
# Check mesh status
mesh-ctl -t tests/simulation/mesh-topology.yml status

# Check transport allocations
mesh-ctl config show

# Ping overlay IPs from master to endpoint (inside container)
docker exec ru-01 ping -c 3 172.20.70.34    # master → kz-01
docker exec ru-01 ping -c 3 172.20.70.37    # master → pl-01

# Check WireGuard interfaces on master
docker exec ru-01 cat /proc/net/dev | grep wg

# Check loopback overlay IP
docker exec kz-01 ip addr show lo | grep 172.20

# Check ECMP routes on master
docker exec ru-01 ip route show | grep 172.20.70
```

## Cleanup

```bash
docker compose -f tests/simulation/docker-compose.yml down -v
```

## Topology

```
                    ┌──────────┐    ┌──────────┐
                    │  ru-01   │    │  ru-02   │
                    │ .50.10   │    │ .50.11   │
                    │ master   │    │ master   │
                    └────┬─────┘    └────┬─────┘
                         │               │
        ┌────────┬───────┼───────┬───────┼───────┐
        ▼        ▼       ▼       ▼       ▼       ▼
   ┌────────┐┌────────┐┌────────┐┌────────┐┌────────┐
   │ kz-01  ││ kz-02  ││ kz-03  ││ pl-01  ││ us-01  │
   │ .50.20 ││ .50.21 ││ .50.22 ││ .50.30 ││ .50.31 │
   │endpoint││endpoint││endpoint││endpoint││endpoint│
   └────────┘└────────┘└────────┘└────────┘└────────┘
```

All on Docker network `mesh-sim` (192.168.50.0/24).
