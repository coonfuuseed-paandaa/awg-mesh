# v1.8.0 Smoke + E2E Test Fixture

Release gate for v1.8.0. These scripts MUST pass before `gh release create v1.8.0`.

## What This Tests

### smoke.sh (< 2 min, no compose required)

| Check | FR | Description |
|---|---|---|
| S0 | — | Build local Docker images from the current worktree |
| S1 | — | Node + client binary loads, `--help` prints without panic |
| S2 | — | `--version` reports a recognizable version string |
| S3 | FR-3 | `dscp: 153` topology rejected by `mesh-ctl routing generate` with DSCP error |
| S4 | FR-3 | `dscp: 10` topology accepted (regression guard) |
| S5 | T006a | `mesh-ctl bootstrap --help` available (skipped if not yet merged) |
| S6 | FR-2 | `--show-token` flag present on `mesh-ctl token rotate` |
| S7 | FR-2 | `--show-token` flag present on `mesh-ctl master prepare` |

### e2e.sh (< 10 min, requires docker compose)

| Check | FR | Description |
|---|---|---|
| E1 | — | `docker compose up` boots 2 masters + 1 endpoint + 1 client |
| E2 | — | Masters report `gRPC server listening` within 60s |
| E3 | — | `mesh-ctl master init` completes cert exchange for master-a + master-b |
| E4 | — | `mesh-ctl endpoint init` establishes endpoint-x tunnel |
| E5 | — | `mesh-ctl client init` installs ECMP routes on client-lin |
| E6 | — | `ping -c 5` from client-lin to endpoint-x overlay IP succeeds |
| E7 | FR-2 | `token rotate` (no flag) — token NOT present on stdout |
| E8 | FR-2 | `token rotate --show-token` — token present on stdout; `event=show_token_flag` in log |
| E9 | FR-4 | Corrupt `node_state.yml` → container restarts cleanly, state replaced |
| E10 | FR-1 | ICMP demux — manual only (see below) |
| E11 | US1 | Kill master-a → client-lin routes via master-b within 35s |
| E12 | US1 | Restart master-a → both nexthops restored within 60s |

---

## Prerequisites

| Requirement | Notes |
|---|---|
| Linux host or WSL2 | macOS Docker Desktop does not support `NET_ADMIN` reliably inside containers |
| Docker Engine 24+ | `docker --version` must report 24.x or newer |
| Docker Compose v2 | `docker compose version` (no hyphen) must report v2.x |
| ~2 GB free disk | Node + client images build from source |
| Kernel with TUN module | `modinfo tun` must succeed |
| `CAP_NET_ADMIN` permitted | Required for `ip route`, AWG TUN device setup |
| `mesh-ctl` in PATH (e2e only) | `go install ./cmd/mesh-ctl` from the repo root |

---

## Quick Start

```bash
# From repo root:

# 1. Build local images
bash tests/v18_smoke/build.sh

# 2. Run smoke checks (< 2 min, no containers)
bash tests/v18_smoke/smoke.sh

# 3. Run full e2e (< 10 min, starts containers)
bash tests/v18_smoke/e2e.sh

# 4. Or: use the Makefile
make smoke-v18
make e2e-v18
make release-gate     # smoke + e2e in sequence — must pass before tag
```

---

## Interpreting Results

Each check prints `[PASS]`, `[FAIL]`, or `[SKIP]`:

- **PASS** — criterion met; commit proceeds.
- **FAIL** — criterion violated; do NOT create the release tag until fixed.
- **SKIP** — feature not yet merged in this build (expected for pre-v1.8.0 worktrees).
  When all v1.8.0 PRs (#37–#41) are merged, there should be zero skips on smoke and zero
  skips on e2e (except E10 which is always manual).

Exit code = number of FAILures. `make release-gate` blocks on non-zero exit.

---

## ICMP Demux (E10) — Manual Procedure

FR-1 (shared ICMP socket + demux map) is tested by the unit test
`TestPingICMPConcurrentDemux` in `pkg/node/health_test.go`. The Docker e2e
cannot replicate the raw-socket fan-out scenario without privileged kernel access
and a traffic generator — it is documented here for manual operator verification:

1. Start the mesh: `docker compose -p v18smoke -f tests/v18_smoke/compose.yml up -d`
2. From a Linux host with `hping3`: inject ICMP noise at master-a:
   ```bash
   hping3 --icmp -i u1000 172.31.10.10 &
   ```
3. From inside client-lin, run concurrent pings:
   ```bash
   docker exec v18smoke-client-lin sh -c '
     for i in $(seq 1 8); do
       ping -c 20 172.20.71.${i} &
     done; wait'
   ```
4. All pings must complete with 0% loss. Any goroutine starvation appears as
   sporadic packet loss on one or more targets.
5. Check `docker logs v18smoke-master-a` for `event=icmp_demux_timeout` —
   zero occurrences is the expected result.

---

## Troubleshooting

| Symptom | Likely cause | Remedy |
|---|---|---|
| `ERROR: docker not running` | Docker daemon stopped | `sudo systemctl start docker` or open Docker Desktop |
| `awg-mesh-node:local-v18 not found` | build.sh not run | `bash tests/v18_smoke/build.sh` |
| `S3 SKIP` | FR-3 not yet merged | Expected on pre-v1.8.0 branch; merge PR #37 |
| `S5 SKIP` | T006a not yet merged | Expected on pre-v1.8.0 branch; merge PR #41 |
| `E3 FAIL: init RPC` | Container not reachable from host | Ensure Docker bridge network `172.31.10.0/24` is not blocked by host firewall |
| `E6 FAIL: ping` | Mesh init failed or tunnel not up | Check E3–E5 logs; `docker logs v18smoke-master-a` |
| `E9 SKIP: no node_state.yml` | Container never wrote state | Run E3–E5 first so nodes are initialized |
| `network v18smoke already exists` | Previous run not cleaned up | `docker compose -p v18smoke -f tests/v18_smoke/compose.yml down -v` |

---

## Cleanup

```bash
docker compose -p v18smoke -f tests/v18_smoke/compose.yml down -v
```

The `e2e.sh` script calls `docker compose down -v` automatically on exit (success or failure).

---

## Architecture

```
172.31.10.0/24 (bridge: v18smoke)
  172.31.10.10  master-a     (overlay: 172.20.71.2)
  172.31.10.11  master-b     (overlay: 172.20.71.3)
  172.31.10.20  endpoint-x   (overlay: 172.20.71.37)
  172.31.10.50  client-lin   (overlay: 172.20.71.130)
```

The overlay space `172.20.71.0/24` is distinct from the existing `tests/client_ecmp`
fixture (`172.20.70.0/24`) and the compose bridge (`172.31.10.0/24` vs `172.31.0.0/24`)
to allow both fixtures to run concurrently without IP conflicts.

---

## Release Gate Usage

This fixture is the Phase 6 gate for v1.8.0. Do not run `gh release create v1.8.0`
until `make release-gate` exits 0:

```bash
make release-gate   # runs smoke-v18 && e2e-v18
# only if exit 0:
gh release create v1.8.0 --title "v1.8.0" --notes "..."
```
