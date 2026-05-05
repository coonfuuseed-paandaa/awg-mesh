# Migration Guide: Legacy MikroTik 5-Container → awg-mesh

This guide walks operators through migrating from the legacy setup — five
MikroTik CHR containers each holding one AWG tunnel, each routing a slice of
downstream traffic — to the `awg-mesh` architecture: two master nodes (ECMP +
session stickiness), N endpoint nodes (AWG server + NAT), and M clients
(Linux/MikroTik/etc.).

## Audience

Operators running the pre-1.0 MikroTik-only topology. If you are on a green-field
deployment, follow `README.md` Quick Start instead — this document is for
in-place migrations with existing traffic.

## Why Migrate

| Concern | Legacy (5× MikroTik) | awg-mesh |
|---|---|---|
| High-availability masters | One container per tunnel; no ECMP | 2+ masters with healthcheck-driven ECMP + sticky sessions |
| Param rotation (anti-DPI) | Manual `/awg set-config` per container | `mesh-ctl rotate --tier {1,2,3}` against live mesh |
| Topology source of truth | Per-container configs out of band | Single `mesh-topology.yml`, declarative |
| gRPC management plane | None (SSH-only) | Dual-auth (mTLS + token), structured API |
| TLS/QUIC packet capture | Out-of-band scripts | Integrated `mesh-ctl capture refresh` |
| Multi-arch support | RouterOS only | Node images: amd64, 386, arm64, arm/v6, arm/v7 |

## Prerequisites

- Running access to the existing five MikroTik CHR containers (API token or
  SSH). Target VPSes for the 2 new master nodes, each with a public IP and
  Docker Engine 20.10+ installed. Admin workstation with `mesh-ctl` installed
  (`go install github.com/coonfuuseed-paandaa/awg-mesh/cmd/mesh-ctl@v1.7.0`).
  Network window of ~30 minutes per endpoint during cut-over.
- Back up every MikroTik CHR's `awg*` interface config
  (`/interface/wireguard export` + `/ip/address export` + `/ip/route export`)
  into a single file per container before touching anything.

## Migration Phases

The migration is **incremental**. You keep the five MikroTik containers
serving traffic while you bring up the two awg-mesh masters alongside them.
Clients are cut over one at a time. Only after every client is healthy on the
new masters do you decommission the old containers.

### Phase 0 — Inventory (≈15 min)

For each of the five legacy containers, capture:

1. The CHR container's external IP and listening port for AWG.
2. The private/overlay IP it advertises to MikroTik clients.
3. The list of clients routing through it.
4. The AWG parameter set in use (`S1-S4`, `H1-H4`, `Jc/Jmin/Jmax`,
   `I1-I5`). Run `/interface/wireguard print detail` and copy the full
   block into an inventory spreadsheet.
5. Any static routes pointing at that container on each MikroTik client.

Write this down. Lose it and you will not be able to roll back cleanly.

### Phase 1 — Provision new masters (≈20 min)

On your admin workstation:

```bash
mesh-ctl master prepare master-a --host 203.0.113.10
mesh-ctl master prepare master-b --host 203.0.113.11
```

Each command produces a `master-<name>/` directory with `docker-compose.yml`,
an initial `mesh-topology.yml` (edit this to match your inventory), and a
one-time init token. The token is saved to
`nodes/master-<name>/token` with mode 0600 — do **not** rely on stdout
(v1.8.0+ hides the token by default; pass `--show-token` only if you need
it on stdout, e.g., for piping to an ops script).

Deploy each master:

```bash
scp -r master-a/ root@203.0.113.10:/opt/
ssh root@203.0.113.10 'cd /opt/master-a && docker compose up -d'
```

Same for master-b. Verify each comes up:

```bash
mesh-ctl status master-a
mesh-ctl status master-b
```

Both must report `grpc: ok`, `tunnels: 0`, `token-auth: active`.

Run `mesh-ctl master init master-a` and `mesh-ctl master init master-b` from
the admin workstation to exchange mTLS certificates. After init, the token
auth becomes inactive and all subsequent operations are mTLS-only.

### Phase 2 — Register endpoints in topology (≈15 min)

The five legacy MikroTik containers map to five *endpoint* definitions in
`mesh-topology.yml` — each MikroTik was effectively acting as an AWG server
terminating one or more client connections, which is the role `awg-mesh-node
--mode endpoint` now plays.

For each legacy container, add to `mesh-topology.yml`:

```yaml
endpoints:
  - name: endpoint-old-1   # choose stable names
    host: 203.0.113.20     # public IP
    overlay_ip: 10.50.0.1  # from your inventory
    listen_port: 51820     # keep or pick a new port
    awg_params:
      s1: <from inventory>
      s2: <from inventory>
      h1: <from inventory>
      h2: <from inventory>
      h3: <from inventory>
      h4: <from inventory>
      jc: <from inventory>
      jmin: <from inventory>
      jmax: <from inventory>
```

The `awg_params` stanza MUST be populated from your Phase 0 inventory.
Leaving it empty lets `mesh-ctl rotate` generate fresh parameters on first
run, but that breaks every connected client until they also get the new
params — do **not** rotate until Phase 4.

### Phase 3 — Deploy endpoints (≈10 min per endpoint)

Migrate one endpoint at a time. For each:

```bash
mesh-ctl endpoint prepare endpoint-old-1 --host 203.0.113.20
scp -r endpoint-old-1/ root@203.0.113.20:/opt/
ssh root@203.0.113.20 'cd /opt/endpoint-old-1 && docker compose up -d'
mesh-ctl endpoint init endpoint-old-1
```

Immediately verify:

```bash
mesh-ctl status endpoint-old-1
# expect: overlay_ip assigned, awg iface up, nat rules present
```

At this point the new `endpoint-old-1` node is **parallel** to the legacy
MikroTik container — both are up, serving the same overlay IP. Traffic on
the legacy container continues undisturbed. No client has been cut over yet.

Repeat for each of the remaining 4 endpoints.

### Phase 4 — Cut over MikroTik clients (≈20 min per client)

For each MikroTik client that currently routes through a legacy container,
update its routing table to send traffic through the new awg-mesh masters
instead of directly to the legacy container:

1. Regenerate the client's AWG config:
   ```bash
   mesh-ctl client prepare client-mikrotik-1 --type mikrotik
   ```
   This produces a `.rsc` script for MikroTik import.
2. On the MikroTik device, `/import file=<generated.rsc>`.
3. Add an ECMP default route through the two masters:
   ```
   /ip/route add dst-address=0.0.0.0/0 gateway=10.50.0.10,10.50.0.11 \
     comment=awg-mesh-ecmp
   ```
   (The masters' overlay IPs come from `mesh-topology.yml`.)
4. Remove the old static route that pointed at the legacy container.
5. Verify traffic flows through the masters:
   ```
   /tool/torch interface=awg-client-1 duration=10
   /ping 8.8.8.8 count=50 interval=0.2s
   ```
   Packet loss must be ≤ 1% during the cut-over window.

**If any client loses connectivity:** restore the previous default route
immediately (`/ip/route add ... gateway=<legacy-container-ip>`) and
investigate before retrying. Legacy containers remain in service throughout
this phase — a bad cut-over is reversible in seconds.

### Phase 5 — Rotate AWG params (optional, ≈5 min)

Once every client is healthy on the new masters, you may want to rotate the
per-client AWG parameters to fresh values. This is **not required** for the
migration itself but is recommended as a hygiene step since the legacy
containers saw those params.

```bash
mesh-ctl rotate --tier 1 --all
```

Tier 1 is a zero-packet-loss rotation via UAPI SET on both sides. If any
client shows >2% packet loss after rotation, re-run with `--tier 2` which
includes a coordinated preflight check.

### Phase 6 — Decommission legacy containers (≈15 min)

**Only after** all clients have been healthy on the new masters for at least
24 hours (longer for production traffic):

1. Drain each legacy container's remaining connections — there should be
   zero by now, but verify via the container's CHR interface:
   `/interface/wireguard/peer print detail`.
2. Stop each legacy container: `/system/shutdown` or `docker stop` on the
   host running CHR.
3. Archive the per-container config backup from Phase 0 to long-term storage
   (S3/archive bucket) — you may need it for forensic work if an audit
   question comes up later.
4. Remove the legacy container from any DNS or load-balancer configuration
   that still references its public IP.

## Rollback Paths

| Stage | What breaks | How to roll back |
|---|---|---|
| Phase 1 (masters up, no clients cut over) | Nothing client-visible | `docker compose down` the masters. Remove from `mesh-topology.yml`. |
| Phase 2 (endpoints registered, not deployed) | Nothing | Discard the topology edits. |
| Phase 3 (endpoints deployed, no clients cut over) | Nothing | `mesh-ctl endpoint remove <name>` on each, then `docker compose down` on the hosts. |
| Phase 4 (mid-cut-over, client regressed) | One client loses connectivity | On that MikroTik: restore the old default route pointing at the legacy container. ≤ 30 seconds impact. |
| Phase 5 (rotate regressed) | All clients using that endpoint | `mesh-ctl rotate --tier 2 --revert`. |
| Phase 6 (legacy stopped, issue discovered) | Severe — legacy gone | Restore legacy container from the Phase 0 config backup. ≈ 10 min per container. |

## Verification Checklist

After Phase 6 completes:

- [ ] `mesh-ctl status` shows both masters healthy, all 5 endpoints up, all
      clients connected.
- [ ] Prometheus metrics from both masters show non-zero RX/TX per tunnel
      (`awg_mesh_tunnel_bytes_rx_total`, `awg_mesh_tunnel_bytes_tx_total`).
- [ ] Every MikroTik client's `/ip/route` lists the ECMP default pointing
      at both masters.
- [ ] No MikroTik client still has a `gateway=<legacy-ip>` route
      (`/ip/route print where comment~"legacy"` returns empty).
- [ ] The 5 legacy CHR containers are stopped or removed.
- [ ] 24-hour capture of master metrics shows stable healthy path count
      equal to total tunnel count — no flapping.

## Operator Gotchas

- **AWG parameters MUST match between master-side and endpoint-side** for
  each tunnel. If you populate them from inventory incorrectly, the tunnel
  will not handshake — logs show `event=awg_handshake_timeout`. Double-check
  the `s1/s2/h1-h4/jc/jmin/jmax` values against the MikroTik
  `/interface/wireguard print detail` output.
- **Overlay IP collisions** between the legacy containers and new
  endpoints are silent — the legacy container "wins" for traffic on the
  physical uplink until you cut it over. Use `mesh-ctl ip list` to confirm
  there are no duplicate overlay IPs between legacy and new entries in
  `mesh-topology.yml`.
- **Token leak in scrollback (pre-v1.8.0):** If you prepared nodes with
  `mesh-ctl` versions before v1.8.0, the one-time bearer token was written
  to stdout and may sit in your shell history or tmux buffer. Rotate it
  after migration: `mesh-ctl token rotate <node>`.
- **MikroTik RouterOS version:** v2.0 client generation uses
  `mesh-ctl node prepare --platform mikrotik` and emits a RouterOS
  `/container` deployment for Linux `awg-mesh-client`. The generator can select
  legacy/transitional/canonical container syntax via `--target-ros`, but the
  current client data plane requires RouterOS 7.21+ container-side nftables
  support. RouterOS 6.x is not supported for this path; native RouterOS vanilla
  WireGuard generation is a deferred future track.
- **Firewalls in front of masters:** the masters listen on the AWG ports
  you configured plus gRPC on `:9090` (mTLS). Open these inbound from
  endpoints + admin workstation only. gRPC should NOT be public.

---

## Rolling Upgrade Procedure (v1.10.2+)

Starting with v1.10.2, `mesh-ctl upgrade` orchestrates a zero-downtime rolling upgrade
of every node in the mesh. This section covers the typical operator workflow.

### Prerequisites

- All nodes running v1.9.0 or later (for compose schema compatibility).
- `mesh-ctl` v1.10.2+ installed on the admin workstation.
- `mesh-topology.yml` up to date.
- If using SSH auto-deploy: passwordless SSH access to each host.

### Older compose files (pre-v1.9.0 nodes)

If any node is running a pre-v1.9.0 docker-compose schema (i.e. it uses a `command:`
block instead of environment variables, or `MESH_TOKEN` instead of `MESH_TOKEN_HASH`),
migrate the compose file first:

```bash
# Detect schema version:
mesh-ctl upgrade compose /etc/docker/compose/<node>-docker-compose.yml

# Migrate in-place (original saved as .bak):
mesh-ctl upgrade compose /etc/docker/compose/<node>-docker-compose.yml --in-place

# Or write to stdout and inspect before applying:
mesh-ctl upgrade compose /etc/docker/compose/<node>-docker-compose.yml > /tmp/migrated.yml
diff /etc/docker/compose/<node>-docker-compose.yml /tmp/migrated.yml
```

> **Note:** If the node has `MESH_TOKEN=<plain>` (schema v1.5.1), the migrated file
> will contain `MESH_TOKEN_HASH=REPLACE_WITH_HASH`. Before deploying, rotate the token
> with `mesh-ctl token rotate <node>` — this generates a new token, bcrypt-hashes it,
> writes the hash to master state, and prints the plain value for the `MESH_TOKEN_HASH`
> substitution on the endpoint.

### Step 1 — Preview the plan

```bash
mesh-ctl upgrade v1.10.2 --dry-run
```

Output shows the ordered node list, roles, regions, and target image for each node.
Endpoints are upgraded first (region-grouped, alphabetical within region), then masters.

### Step 2 — Execute the upgrade

**With SSH auto-deploy** (recommended for unattended use):

```bash
mesh-ctl upgrade v1.10.2 \
    --ssh \
    --ssh-user deploy \
    --ssh-key ~/.ssh/mesh_deploy_ed25519 \
    --downtime-budget 120 \
    --deploy-wait 180
```

**Manual deploy** (if SSH access is restricted):

```bash
mesh-ctl upgrade v1.10.2 --deploy-wait 300
```

For each node the CLI prints the compose file path. Copy it to the host and run
`docker compose up -d` manually. The CLI polls gRPC until the node reports ready
(up to `--deploy-wait` seconds) before moving to the next node.

### Step 3 — Monitor progress

In a second terminal:

```bash
watch mesh-ctl upgrade status
```

The JSONL audit log is written to `~/.mesh-ctl/upgrade-v1.10.2-<timestamp>.log`.

### Rollback behaviour

If the `verify` phase fails for a node (data-plane probes detect packet loss), the
driver automatically:

1. Restores the pre-upgrade compose from the `.bak` snapshot.
2. Re-runs `docker compose up -d` (if SSH is enabled) or prints the rollback path.
3. Polls gRPC until the node is ready again.
4. Re-runs mesh reconciliation.

The rolling upgrade halts at the failed node. Nodes upgraded earlier are left at the
new version; rolled-back nodes return to their previous version.

To check rollback status after a partial upgrade:

```bash
mesh-ctl upgrade status
```

### Override upgrade order

```bash
# Upgrade two specific nodes only (manual order):
mesh-ctl upgrade v1.10.2 --dry-run --order ep-eu-1,master-eu-1

# Check the plan, then apply without --dry-run.
```

---

## See Also

- `README.md` — high-level architecture and quick start.
- `README.ru.md` — Russian translation of the above.
- `README.zh-CN.md` — Simplified Chinese translation of the above.
- `docs/adr/0001-multi-image-docker-strategy.md` — why masters and clients
  ship as separate container images.
- `docs/adr/0002-mikrotik-veth-interface-discovery.md` — how the MikroTik
  client integrates with RouterOS networking.
- `docs/adr/0004-deterministic-client-interface-naming.md` — v1.7.0 change
  that affects Prometheus scrape labels if you monitor interfaces by name.
- `docs/adr/0005-transport-state-schema-versioning.md` — why your existing
  `transport.yml` continues to load despite the schema bump.

## Support

File issues at https://github.com/coonfuuseed-paandaa/awg-mesh/issues with
the `migration` label. Include: your starting version (legacy build tag),
your target version (`mesh-ctl version` output), and the stage at which
you got stuck.
