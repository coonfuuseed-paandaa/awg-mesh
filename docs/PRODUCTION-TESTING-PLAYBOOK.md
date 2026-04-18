# Production Testing Playbook — Local Docker Emulation

End-to-end integration testing for `awg-mesh-node` + `mesh-ctl` against a
Docker-based mesh simulation. Use this before shipping any PATCH/MINOR release
that touches the control plane (gRPC handlers, bootstrap, reconcile) or data
plane (WireGuard interface management, routing, DSCP).

## Scope

What this playbook validates:

- Multi-master + multi-endpoint + multi-client mesh boots cleanly.
- gRPC control-plane operations (prepare, init, reconcile, upgrade) work
  across separate container filesystems.
- Data-plane tunnels actually carry traffic — overlay-IP ping + WG handshakes.
- PATCH/MINOR release artefacts are compatible with the existing mesh state
  (no schema breakage, no rotation regressions, no silent stub paths).

What this playbook does NOT validate:

- Production-grade latency or throughput — Docker bridge network has its own
  performance profile.
- Cross-region behaviour — all containers are on one host.
- Real internet conditions (packet loss, MTU discovery, DPI) — use a VPS
  canary for that after local emulation passes.

## Prerequisites

| Requirement | How to verify |
|------------|--------------|
| Docker Engine ≥ 24 | `docker info` shows `Server Version: …` |
| Linux kernel with WireGuard module | `modprobe wireguard && lsmod \| grep wireguard` |
| Go 1.25+ in WSL (if on Windows) | `wsl bash -c 'go version'` |
| `mesh-ctl` installed | `which mesh-ctl && mesh-ctl version` |
| `awg-mesh-node` image built locally | `docker images awg-mesh-node` lists at least one tag |

### Windows-specific

Docker Desktop on Windows runs Linux containers via a Linux-VM (WSL2 or
Hyper-V). The WireGuard kernel module IS available in the Docker VM, so data
plane tests work. Scripts guard `uname -s != Linux` with SKIP — run them from
inside WSL2 Ubuntu (not PowerShell) so `uname -s` returns `Linux`:

```bash
wsl bash -c 'cd /mnt/d/Dev/awg-mesh && bash tests/simulation/issue-92-rotation.sh'
```

## Available Simulations

| Script | Validates | Release scope |
|--------|-----------|--------------|
| `tests/simulation/docker-compose.yml` | Full 7-node mesh topology (2 masters + 5 endpoints + 1 client) | Baseline smoke test |
| `tests/simulation/issue-92-rotation.sh` | Endpoint key rotation propagates to masters via `UpdateTunnelPeer` RPC within 5s | PR #51 (v1.10.0) |
| `tests/simulation/issue-93-upgrade.sh` | 5-phase guided upgrade with rollback on health failure | PR #53 (v1.10.2) |
| `tests/simulation/issue-100-scp-compose.sh` | SFTP compose upload hook wiring (unit-level) | PR #59 (v1.11.0) |
| `tests/simulation/e2e_test.go` | Go integration test (e2e_test.go) — boot + basic gRPC reachability | Per-release |

Note: all shell scripts write their own throwaway `docker-compose.yml` to
`/tmp/issueNNrot-compose-XXXXXX.yml` and use distinct COMPOSE_PROJECT names
to avoid colliding with other running sims.

## Workflow

### Phase 1 — Build the image from the branch under test

Build the image from the specific worktree. Tag both `awg-mesh-node:local`
(default script image) and a branch-specific tag for later reference.

```bash
# From the worktree root:
docker build \
  -f deploy/Dockerfile.node \
  -t awg-mesh-node:local \
  -t awg-mesh-node:v1.10.4-pr58 \
  --build-arg VERSION=v1.10.4-pr58 \
  .
```

Record the image digest:

```bash
docker inspect awg-mesh-node:v1.10.4-pr58 --format '{{.Id}}'
```

### Phase 2 — Baseline smoke test (docker-compose)

```bash
cd tests/simulation
docker compose up -d
docker compose ps  # expect 7 containers running
```

Initialize the mesh (requires `mesh-ctl` on the host):

```bash
for name in node-asia-01 node-asia-02 node-asia-03 node-eu-01 node-us-01; do
  mesh-ctl -t mesh-topology.yml endpoint prepare --name $name
done
mesh-ctl -t mesh-topology.yml master prepare --name master-01
mesh-ctl -t mesh-topology.yml master prepare --name master-02

for name in node-asia-01 node-asia-02 node-asia-03 node-eu-01 node-us-01; do
  mesh-ctl -t mesh-topology.yml endpoint init --name $name
done
mesh-ctl -t mesh-topology.yml master init --name master-01
mesh-ctl -t mesh-topology.yml master init --name master-02
```

Verify:

```bash
# Mesh status — all nodes ONLINE, transport tunnels populated
mesh-ctl -t mesh-topology.yml status

# Data-plane ping — master can reach every endpoint via overlay
docker exec master-01 ping -c 3 172.20.70.34
docker exec master-01 ping -c 3 172.20.70.37

# Reverse ping — endpoint can reach master
docker exec node-asia-01 ping -c 3 172.20.70.2

# ECMP route on master — should list multiple nexthops per endpoint subnet
docker exec master-01 ip route show | grep 172.20.70

# WG peer list on master — expect one entry per endpoint with fresh handshake
docker exec master-01 wg show
```

Cleanup:

```bash
docker compose down -v
```

### Phase 3 — Targeted regression sims

Run the dedicated scripts for the area your PR touches. Each script is
self-contained: spins up its own topology, exercises the scenario, verifies,
tears down.

```bash
bash tests/simulation/issue-92-rotation.sh    # key rotation propagation
bash tests/simulation/issue-93-upgrade.sh     # guided upgrade with rollback
```

Each script exits:
- `0` — all checks passed
- non-zero — failure count; stderr has the specific check that failed

Each script takes ~30-60 seconds end-to-end on a warm Docker daemon.

### Phase 4 — Branch-specific smoke tests

After phase 2+3 pass on the branch's image, run the ad-hoc smoke check for
the specific feature the PR introduces. Examples:

**PR #58 (per-peer mutex + DSCP teardown invariant):**

```bash
# Exercise AddPeer existing-link path (mutex coverage):
mesh-ctl -t mesh-topology.yml endpoint init --name node-asia-01   # first call
mesh-ctl -t mesh-topology.yml endpoint init --name node-asia-01   # re-init
# Should succeed both times, no deadlock, no orphan WG interfaces.

# Exercise DSCP teardown on clean-slate node:
docker exec master-01 /usr/local/bin/awg-mesh-node --help  # tear down happens on SIGTERM
docker restart master-01
docker logs --tail 30 master-01 | grep -i dscp
# Expect: "awg_dscp table absent; continuing" on first boot (ENOENT path).
```

**PR #59 (SFTP compose upload):**

```bash
# Requires sshd in target container (not in default sim image — use the
# integration test path in tests/simulation/issue-100-scp-compose.sh).
bash tests/simulation/issue-100-scp-compose.sh
```

### Phase 5 — Version tag verification

After the image passes all sims, verify the baked-in version matches the
expected build:

```bash
docker run --rm awg-mesh-node:local --version
# Expect: awg-mesh-node vX.Y.Z-prNN or main@<sha>
```

Cross-check against `git describe`:

```bash
git describe --tags HEAD
```

## Clean slate

If sims corrupt state or hang, nuke everything:

```bash
docker ps -a --format '{{.Names}}' | grep -E 'issue[0-9]+|mesh-sim|master-0|node-|client-' | xargs -r docker rm -f
docker volume ls -q | grep -E '^(master-|node-|client-)' | xargs -r docker volume rm
docker network ls -q --filter 'name=mesh-sim\|issue' | xargs -r docker network rm 2>/dev/null || true
```

## When this playbook is enough

Local Docker emulation covers:
- gRPC wire compatibility
- Bootstrap flow end-to-end (prepare → init → reconcile)
- Data-plane tunnel handshake + overlay ping
- Key rotation propagation
- Upgrade flow with rollback

Local Docker emulation does NOT cover — needs a VPS canary:
- Real latency, real MTU discovery, real packet loss
- Cross-region routing quality
- DPI / ISP interference
- Systemd integration (Docker containers use tini/sh as PID 1, not systemd)
- Operator workflows that require SSH to a different host (use the VPS canary
  for `mesh-ctl upgrade --ssh` multi-host validation)

## Known gaps (2026-04-18)

The following simulation entry points need refresh before they can be relied
on in CI. These were working in older release cycles but drifted as the token
/ auth flow evolved:

- `tests/simulation/issue-92-rotation.sh` — last updated by PR #51 (v1.10.0
  UpdateTunnelPeer). Script writes `test-token-placeholder` into the
  container's `/config/mesh.token` before `mesh-ctl master prepare` generates
  the actual token-hash in the admin config dir. `master init` then fails
  with `Unauthenticated desc = authentication required` because the token
  mesh-ctl holds does not match the one embedded in the container. Needs:
  either (a) pre-run `mesh-ctl master prepare` and copy resulting
  `${CTL_CONFIG_DIR}/nodes/${MASTER}/token` into each container BEFORE
  `docker compose up`, or (b) adopt a deterministic test-only token
  convention shared between mesh-ctl and the container entrypoint.

- `tests/simulation/issue-93-upgrade.sh` — likely exhibits the same auth
  gap; not re-verified this session.

Fix ETA: v1.10.5 bucket alongside #103/#105 reconcile/status fixes. Tracked
internally. Until then, use local Docker only for manual end-to-end smoke
against a freshly-built image via the `docker-compose.yml` + manual
`mesh-ctl` sequence in Phase 2.

## Playbook compliance gate

A PR that touches data plane or bootstrap MUST record:
- [ ] Phase 1 image built — which tag and digest
- [ ] Phase 2 smoke test ran — all 7 containers ONLINE, overlay ping passed
- [ ] Phase 3 relevant sim ran — which script, exit 0
- [ ] Phase 4 branch-specific check ran — what was verified

Record in the PR description or in `.agent/reports/sim-<branch>-<date>.md`.

## References

- `tests/simulation/README.md` — per-topology quick reference
- `tests/simulation/docker-compose.yml` — 2 masters + 5 endpoints + 1 client
- `tests/simulation/mesh-topology.yml` — overlay IP + transport subnet layout
- `AGENTS.md` — project-level release policy (every tag must have GHCR image)
- `.agent/CONTINUITY.md` — current PRs + release state
