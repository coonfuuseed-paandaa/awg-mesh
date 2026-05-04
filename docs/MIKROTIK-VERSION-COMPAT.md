# MikroTik RouterOS Version Compatibility Playbook

**Audience:** awg-mesh `mesh-ctl` developers + operators choosing target RouterOS for MikroTik clients.
**Goal:** define which RouterOS 7.x versions awg-mesh supports, which `/container/*` syntax pivots the `.rsc` generator (`pkg/mikrotik`) MUST cover, and which RouterOS versions can run the current client container data plane.

**v2.0 scope note:** the release gate for MikroTik is the RouterOS container
path (`awg-mesh-client` inside `/container`). Native RouterOS vanilla
WireGuard support is intentionally deferred and must not be used as the current
release blocker.

All facts are verified against MikroTik's official changelogs at `https://download.mikrotik.com/routeros/<version>/CHANGELOG`. Inline citations point to the exact changelog entry.

---

## 1. Supported version range

| Range | Generator syntax tier | Runtime/data-plane tier | Why |
|-------|-----------------------|-------------------------|-----|
| **< 7.4** | NOT SUPPORTED | NOT SUPPORTED | `/container` package does not exist. v6 is dead-end. |
| **7.4 (testing)** | NOT SUPPORTED | NOT SUPPORTED | Container available only in `testing` channel (per 7.4 CHANGELOG: *"Container package is not available in v7.4. Development and testing continues in 'testing' channel."*). Containers must be recreated against 7.5 anyway. |
| **7.5 — 7.17** | **TIER 2 (legacy syntax)** | NOT SUPPORTED for current awg-mesh-client data plane | First stable container release (per 7.5 CHANGELOG: *"added support for running Docker (TM) containers on ARM, ARM64 and x86 (containers created before v7.4 must be recreated)"*). Pre-`remote-image` and pre-`mountlists` dialect. Generator MUST emit legacy form, but RouterOS before 7.21 does not provide the container-side `nf_tables` support the v2 client data plane requires. |
| **7.18 — 7.20** | **TIER 1.5 (transitional)** | NOT SUPPORTED for current awg-mesh-client data plane | `remote-image=` introduced (7.18 CHANGELOG: *"allow specifying registry using remote-image property"*). Mount reference parameter still called `mounts=`. `envlist=` accepts multiple values starting 7.20. Generator MAY emit either dialect, but runtime data-plane support still starts at 7.21. |
| **7.21+** (LTS) | **TIER 1 (canonical)** | **SUPPORTED** | Mount reference parameter renamed `mounts=` -> `mountlists=` (7.21 CHANGELOG: *"convert container mounts setting to mountlists, old mount name becomes list name, list name can map to multiple mounts"*). `/app` menu introduced. Container-side nftables support is present for the current awg-mesh-client data plane. **awg-mesh canonical runtime target.** |
| **7.22.x** | AVOID - known regression | NOT SUPPORTED until runtime self-heal lands | Default `ip rule` priorities inside containers regressed to consecutive low (1/2/3), breaking DSCP policy routing for awg-mesh client. See §3.3. Reverted in 7.23rc2. |
| **7.23+** | TIER 1 (canonical, regression-free) | SUPPORTED | Same dialect as 7.21 (mountlists/list/remote-image). `ip rule` priorities restored. |

**Current v2.0 behavior:** `mesh-ctl node prepare --platform mikrotik`
generates RouterOS `/container` scripts with per-tier dialect selection via
`--target-ros <version>`. The default target is the current canonical dialect
(`7.21+`). Generator tests must cover legacy, transitional, and canonical
syntax pivots; real CHR runtime validation is scoped to RouterOS `7.21+`
because the current client data plane requires container-side `nf_tables`.
The topology field `target_ros:` is not implemented yet; use the CLI flag for
per-deploy selection.

---

## 2. Syntax pivot table

The generator (`pkg/mikrotik/commands.go`) MUST switch dialects based on a target version. There are exactly **two** breaking pivots:

| Concern | 7.5 — 7.17 (legacy) | 7.18 — 7.20 (transitional) | 7.21+ (canonical) |
|---------|---------------------|----------------------------|-------------------|
| **Container image source** | `image=docker.io/...` | `image=...` OR `remote-image=...` | `remote-image=...` (preferred) |
| **Mount reference on `/container/add`** | `mounts=NAME` | `mounts=NAME` | `mountlists=NAME` |
| **Mount entry creation** | `/container/mounts/add name=NAME src=… dst=…` | `/container/mounts/add name=NAME …` | `/container/mounts/add list=NAME …` ¹ |
| **Env list reference** | `envlist=NAME` | `envlist=NAME` (multiple OK from 7.20) | `envlist=NAME,NAME2` |
| **`start-on-boot` parameter** | from 7.6 | yes | yes |
| **Read-only mounts** | NO | NO | yes (from 7.20) |
| **Per-container `layer-dir`** | NO | NO | yes (from 7.21) |
| **Multiple veths per container** | NO | NO | yes (from 7.20) |
| **veth `dhcp=yes`** | NO | NO | yes (from 7.20) |

¹ The 7.21 changelog phrasing *"old mount name becomes list name"* implies the same parameter slot was renamed. Verified empirically against CHR 7.21.4 in `tests/simulation/mikrotik-chr-import.sh`. Pre-7.21 CHRs treat `list=` as unknown — they expect `name=`.

**Sources** (per-version changelog):
- 7.5: `/container` first stable — image= syntax baseline.
- 7.6: `start-on-boot` (*"added 'start-on-boot' parameter for automatic container startup"*).
- 7.7: only fixes (no breaking change).
- 7.8: registry auth — `username=`/`password=` on `/container/config` (*"added authentication option for registry (CLI only)"*).
- 7.10: OCI manifest pull support (no syntax change).
- 7.11beta5: veth IPv6 + multiple addresses (no syntax breakage).
- 7.18: **`remote-image=` introduced** (*"allow specifying registry using remote-image property"*); `/ip/firewall` matchers expanded for postrouting (*"allow in-interface/in-bridge-port/in-bridge matching in postrouting chains"*).
- 7.20: cgroups (`cpuset`/`cpu`/`memory`/`pids`), `device=`, `/container/shell`, multiple envlists (*"allow to specify multiple envlists"*), read-only mounts, mount individual files, KVM accel, veth `dhcp=`/`mac-address=`.
- 7.21: **`mounts` → `mountlists` rename**, `/app`, `hosts=`, `kill`, `stop-time`, `update`, per-container `layer-dir`.

---

## 3. nftables — two distinct concerns (split here)

There are TWO separate nftables questions for awg-mesh on RouterOS. They have different answers and different version pivots.

### 3.1 RouterOS host-side `/ip/firewall/*` CLI — **stable across 7.x, no version branching**

RouterOS v7 ships a kernel-level `nf_tables` backend used internally by `/ip/firewall`. **No CHANGELOG entry mentions a user-facing CLI rewrite of `/ip/firewall`** between 7.4 and 7.21 — the table/chain/action grammar is stable across the entire 7.x series.

What did change inside the engine (transparent to operators):
- 7.18: postrouting chain gained `in-interface`, `in-bridge-port`, `in-bridge` matchers. Earlier versions reject those keys in srcnat/postrouting.
- All versions accept `action=fasttrack-connection` and `action=accept`/`drop` with identical semantics.

**Generator implications:**
- `awg-mesh` does NOT use postrouting in-bridge matchers, so the 7.18 expansion is irrelevant.
- The fasttrack anchor logic in `pkg/mikrotik/commands.go` (`/ip/firewall/filter find where action=fasttrack-connection chain=forward` then `place-before=$fastTrackId`) works identically on 7.5 — 7.21+.
- **No version branching needed for host-side firewall.** A single template covers the whole supported range.

### 3.2 Netfilter capabilities INSIDE containers — partial, evolving, with one known regression

This is the actually load-bearing question for awg-mesh: our client image (`deploy/Dockerfile.client`) installs `nftables` + `iptables-legacy` and uses both at runtime — see `pkg/node/client_linux.go::setupClientFirewallRules` (NAT srcnat + TCP MSS clamp) and `setupDSCPRouting` (`ip rule` priorities + nftables `mangle` table for DSCP marking). If the kernel modules backing these are absent inside the RouterOS container, `awg-mesh-node` in client mode silently degrades.

**Confirmed AVAILABLE in RouterOS containers (per MikroTik forum operator reports + awg-mesh runtime testing):**

| Capability | nftables module | iptables-legacy equivalent | Used by awg-mesh client |
|------------|-----------------|---------------------------|-------------------------|
| Stateless filter | `nf_tables`, `nft_compat` | `ip_tables`, `iptable_filter` | yes (FORWARD ACCEPT) |
| NAT srcnat masquerade | `nf_nat`, `nft_nat`, `nft_chain_nat` | `iptable_nat`, `nf_nat_masquerade_ipv4` | **yes** (F-002 T-002) |
| TCP MSS clamping | `nft_exthdr`, `nft_payload` | `xt_TCPMSS` | **yes** (F-002 T-002) |
| Connection tracking match | `nf_conntrack`, `nft_ct` | `xt_conntrack` | yes (sticky ECMP) |
| `ip rule` policy routing | (kernel `fib_rules`) | (same) | **yes** (DSCP routing) |

**Confirmed UNAVAILABLE in RouterOS containers** (per [forum.mikrotik.com — feature request 160725](https://forum.mikrotik.com/t/feature-request-the-container-fully-supports-iptables-and-nftables-internally/160725), open since Sep 2022, still unresolved as of Oct 2025):

| Missing module | What it would enable |
|----------------|----------------------|
| `xt_TPROXY` | Transparent proxy (Tailscale, sing-box `tproxy` mode) |
| `nft_tproxy` | nftables tproxy expression |
| `nft_socket` | Socket match for tproxy backflow |

awg-mesh does NOT use TPROXY or socket match — we route via `ip rule` + standard NAT, not via tproxy. **None of the missing modules block awg-mesh.**

### 3.3 Known regression: ROS 7.22 `ip rule` priorities inside containers

[forum.mikrotik.com — RouterOS 7.22+ Container TUN Gateway Broken](https://forum.mikrotik.com/t/routeros-7-22-regression-containerized-sing-box-tun-gateway-works-on-7-20-8-but-fails-on-7-22/269160):

> RouterOS 7.22 changed default `ip rule` priorities inside containers from well-spaced (200, 2147483646, 2147483647) to consecutive low (1, 2, 3). Apps that need to insert custom priority-based rules end up at the bottom of the lookup table — ineffective.

**Impact on awg-mesh:** our DSCP policy routing (`pkg/node/client_linux.go::setupDSCPRouting`) inserts custom `ip rule` entries at user-controlled priorities (default 100/110/120 per DSCP class). If the container ships with reserved low priorities 1/2/3 and our 100-range entries land BELOW the kernel defaults, DSCP-marked traffic will not match our rule and will fall through to main table.

**Resolution timeline:**
- 7.22 (broken)
- 7.23rc2: `route - revert to old routing rule priorities for containers (introduced in v7.22)` — fixed.

**Two-layer remediation** (per project policy: self-heal in the container; do
not add operator flags for runtime quirks):

1. **Generator (offline) — implemented in the v2.0 candidate:** refuses
   `--target-ros 7.22.x` with an operator-friendly error pointing to 7.21 LTS
   or 7.23+. No silent generation against a known-broken target.
2. **Container runtime — future work:** `awg-mesh-node` client mode should probe
   `ip rule show` at startup. If `pref 1` / `pref 2` / `pref 3` are detected on
   `local`/`main`/`default` (signature of 7.22 regression), the runtime should
   normalise them to `200` / `2147483646` / `2147483647` before installing DSCP
   rules. Idempotent, non-fatal on `ENOENT`. Logs a warning naming the
   regression. Operators running RouterOS 7.22 with an awg-mesh-client
   container today must downgrade the host to 7.21 LTS or upgrade past 7.23
   before the client's DSCP routing will work.

(Earlier playbook v1 dismissed all nftables concerns. That was wrong for the container side. Section 3.1 still holds for host CLI; sections 3.2 + 3.3 are the load-bearing parts for client containers running under RouterOS.)

---

## 4. Architecture — two layers, no auto-deploy

awg-mesh splits MikroTik compatibility across two layers. Generator stays offline + operator-driven; container handles runtime kernel quirks autonomously. **No auto-deploy** — operator configurations differ too much (CRS vs hAP vs CHR, custom firewall, VLAN trunks, IPv6 layouts) to safely SSH and apply changes.

### 4.1 Generator (offline, operator-driven) — current v2.0 shape

`mesh-ctl node prepare --platform mikrotik <name>` produces a RouterOS
deployment bundle under the prepared node directory. Operator copies + imports
the generated `.rsc` manually.

**Bundle layout:**
```text
<config-dir>/nodes/<name>/
  routeros.rsc               # .rsc for the chosen RouterOS tier
  token                      # raw registration token for operator custody
  mesh.token                 # hashed token consumed by the client container
  node.crt / node.key        # node mTLS identity embedded into RouterOS envs
```

**Configuration sources (priority — highest wins):**

1. CLI flag `--target-ros 7.16.2` (per-invocation override)
2. Built-in default: current canonical dialect (`7.21+`)

`mesh-topology.yml` `target_ros:` is intentionally not wired in this release.
Add it only when a persistent per-node target is required by real operator
workflows.

**ContainerConfig:**

```go
type ContainerConfig struct {
    // ... existing fields ...

    // TargetROSVersion is the minimum RouterOS version the generated .rsc
    // must import cleanly on. Default: latest LTS (currently 7.21). Accepts
    // 7.5.0+. Drives dialect selection: legacy (7.5-7.17), transitional
    // (7.18-7.20), canonical (7.21+, 7.23+). Refuses 7.22.x — known regression.
    TargetROSVersion string
}
```

Helper `selectMikrotikDialect(version) Dialect` → `DialectLegacy` / `DialectTransitional` / `DialectCanonical`. Validation rejects:
- pre-7.5 (no container support)
- 7.22.x (known `ip rule` regression — operator-friendly error pointing to 7.21 LTS or 7.23+)

**Per-tier syntax:**

```text
imagePart(cfg) =
    DialectLegacy        → "image="        + cfg.Image
    DialectTransitional  → "remote-image=" + cfg.Image  (image= still accepted, prefer canonical name)
    DialectCanonical     → "remote-image=" + cfg.Image

mountListRef(cfg) =
    DialectCanonical → "mountlists=" + cfg.MountName
    else             → "mounts="     + cfg.MountName

mountCreateLine(cfg) =
    DialectCanonical → "/container/mounts/add list=NAME src=... dst=..."
    else             → "/container/mounts/add name=NAME src=... dst=..."
```

### 4.2 Container runtime (self-healing inside `awg-mesh-node`) — future work

The probe matrix below is not wired into v2.0. The 7.22 ip-rule normalisation is
documented in §3.3 as future runtime hardening; the current release gate avoids
7.22.x at generator level.

Generator decides `.rsc` syntax. Container handles everything observable from inside the namespace — kernel features, ip-rule layout, netfilter backend availability. No operator flags, no topology fields. Detect → log → remediate-or-fallback. Idempotent.

**Init probes** (run by `awg-mesh-node` client mode at startup, before `setupClientFirewallRules` / `setupDSCPRouting`):

| Probe | Detection | Remediation |
|-------|-----------|-------------|
| 7.22 ip rule regression | parse `ip rule show`; pref 1/2/3 hold local/main/default | del pref 1/2/3 → add pref 200/2147483646/2147483647 |
| nftables backend availability | `nft list ruleset` exit code | OK → use nftables; FAIL → fallback iptables-legacy (already in Dockerfile) |
| `nf_nat` masquerade module | dry-add postrouting masquerade chain in test table | OK → enable NAT; FAIL → warn + skip NAT (degraded mode) |
| `/dev/net/tun` | stat | OK → continue; ABSENT → fail-fast with clear error (cannot run amneziawg-go) |
| Conntrack match | dry-add ct match rule | OK → enable sticky ECMP; FAIL → warn + skip |

Runtime emits a startup table summarising detected features for operator visibility:
```text
client-init: kernel features:
  nftables=yes  iptables-legacy=yes  nf_nat=yes  conntrack=yes  tun=yes
  ip-rule-regression=detected-and-fixed (was pref 1/2/3, now 200/big-1/big)
```

### 4.3 Why no auto-deploy

Operators run RouterOS in heterogeneous setups: hAP/CRS hardware, CHR on KVM/VMware/Proxmox, custom firewall layers, VLAN trunks, MPLS, dual-WAN failover, IPv6-only management. SSH-and-apply cannot anticipate enough corner cases to be safe. We give operator a deterministic `.rsc` + `INSTRUCTIONS.md`; they decide where it fits in their config.

---

## 5. Test architecture — CHR-only, alpine proxy DEPRECATED

awg-mesh's MikroTik integration runs against **real RouterOS CHR via QEMU/KVM** in Docker. The earlier alpine-proxy sim (`tests/simulation/mikrotik-onboard.sh`) is DEPRECATED — emulation is dishonest, syntax + container + firewall behaviour live in actual RouterOS userspace, not a Linux container with veth pre-injected from the host.

### 5.1 Three-layer sim architecture

```text
┌─ Host (WSL2 / native Linux / CI runner) — needs only KVM ─┐
│                                                            │
│  Docker network: chr-e2e-net-<suffix> (Docker IPAM)        │
│  ├── master-01     (Docker Linux)  — mesh master           │
│  ├── master-02     (Docker Linux)  — mesh master           │
│  ├── endpoint-01   (Docker Linux)  — mesh endpoint         │
│  └── chr-mikrotik  (Docker QEMU)   — real RouterOS CHR     │
│       ↑                                                    │
│       │ SSH + SCP from host                                │
│       │ /import deploy.rsc                                 │
│       │ /container start awg-mesh-client                   │
│       │                                                    │
│       └── INSIDE CHR userspace:                            │
│           /interface/veth/add → BR_AWG_MESH                │
│           /container/awg-mesh-client (running)             │
│           handshake → master-01/02 over overlay            │
│           172.21.92.34/27 → ping endpoint-01 ↔ CHR ↔ ...   │
└────────────────────────────────────────────────────────────┘
```

No netns injection from host — everything happens inside CHR via RouterOS native
CLI. Host needs `/dev/kvm` plus a VM/QEMU control path that RouterOS accepts as
x86 reset/cold-reboot confirmation for `device-mode container=yes`.

### 5.2 Baseline CHR image cache

Runtime CHR E2E uses pre-baked baseline Docker images for RouterOS `7.21+`
targets, defaulting to `awg-mesh-chr-baseline:7.21.4`. Built ONCE per dev
machine / CI cache. Each baseline contains:

| State | Why |
|-------|-----|
| Admin password set (`lintpass`) | Skip first-boot password dance every run |
| `container-${VERSION}.npk` installed | Upstream CHR image ships the RouterOS system package only; `/container` is an extra package |
| `/system/device-mode` `container=yes` confirmed after cold reboot | RouterOS requires physical confirmation or x86 cold reboot before container functionality is enabled |
| `/container/print` succeeds | Container support verified before snapshot |
| Container config set (`registry-url=...`, `memory-high=...`) | Keep container defaults deterministic |
| Local image import path verified | The sim uploads `awg-mesh-client:local` as a tarball and imports it with `/container/add file=...`, avoiding registry dependence |

Builder script: `tests/simulation/lib/build-chr-baseline.sh CHR_VERSION=7.21.4`. Idempotent only for images carrying label `awg-mesh.chr-container-enabled=true`; older or failed baseline images are rebuilt instead of silently reused.

If the builder fails with `/system/device-mode` still showing `container: no`
and `attempt-count > 0`, the CHR host did not provide a reset/power event that
RouterOS accepts as x86 device-mode confirmation. This is a release-blocking
environment failure, not a reason to weaken the gate: rebuild on a host or
hypervisor path that can trigger an accepted CHR reset/power cycle, then rerun
the runtime matrix.

Pre-7.21 CHR versions (`7.16.2`, `7.20.8`) are generator syntax pivots, not
runtime data-plane targets for v2.0. They are covered by `pkg/mikrotik` tests
unless a separate import-only CHR gate is added.

### 5.3 Bare RouterOS runtime baseline (`mikrotik-chr-baseline-runtime.sh`)

Before importing `awg-mesh-client`, the release gate runs a bare RouterOS
runtime baseline:

```text
1. Verify the labeled CHR baseline image exists and reports
   `/system/device-mode container=yes` plus the installed `container` package.
2. Boot CHR through docker-routeros in two-network mode: `eth0` stays on QEMU
   hostfwd for SSH/API, `eth1` is attached to the simulation network.
3. Start minimal Linux probe targets outside RouterOS.
4. Apply the documented RouterOS container LAN: veth, bridge, bridge gateway,
   srcnat masquerade, and forward accept rules.
5. Import a tiny Linux probe image, start it with `logging=yes`, and verify
   `/log/print where topics~"container"` includes the probe output marker.
6. Assert NAT and forward firewall packet counters increase after
   container-originated traffic.
7. Assert the exact future control-plane address is reachable from the
   RouterOS/container path before product deploy starts.
```

On Docker Desktop/WSL, docker-routeros may expose `eth1` to the CHR guest but
still fail guest-to-neighbour-container traffic on the shared Docker network.
The baseline records that as a warning and falls back to the QEMU slirp proxy
for bootstrap reachability. Set `REQUIRE_SHARED_NETWORK=1` when the goal is a
hard data-plane proof for the shared-network path.

### 5.4 Per-test product runtime sim flow (`mikrotik-chr-e2e.sh`)

```text
1. Pre-flight: docker, KVM, baseline image, `awg-mesh-node:local`,
   `awg-mesh-client:local`, and `mesh-ctl` are present.
2. Run `mikrotik-chr-baseline-runtime.sh`; product deploy is not attempted
   until the bare RouterOS runtime gate passes.
3. Create an isolated Docker bridge network for this run.
4. Generate a minimal v2 topology with one Linux master/control-plane node and
   one MikroTik client node.
5. Run `mesh-ctl node prepare master-01`.
6. Run `mesh-ctl node prepare --platform mikrotik --control-plane <docker-gateway-ip>:<published-grpc-port> mtk-home`.
7. Start a Linux `awg-mesh-node --mode control-plane` container and register
   both prepared nodes with `mesh-ctl node init`.
8. Assert the generated `.rsc` defines `/container/add` for `awg-mesh-client`
   and does not contain `/interface/wireguard`.
9. Build a RouterOS-compatible single-platform client image archive via
   `docker buildx build --platform linux/amd64 --provenance=false --output type=docker,dest=...`,
   upload the tarball to CHR, and rewrite the generated script from
   `remote-image=...` to `file=awg-mesh-client.tar` for the local import test.
10. Bring up CHR from baseline. The container is created on Docker's default
   bridge and connected to the test network before start; this gives
   `evilfreelancer/docker-routeros` both `eth0` for QEMU `hostfwd` SSH/API
   and `eth1` for the RouterOS data-plane NIC. The harness pins
   `ROUTEROS_NIC_MAC` so the CHR disk sees a stable NIC identity across the
   baseline build and each E2E run:
      docker create --network name=bridge,driver-opt=com.docker.network.endpoint.ifname=eth0 \
                    -e ROUTEROS_NIC_MAC=3e:b1:b2:e4:28:54 \
                    --device /dev/kvm --device /dev/net/tun \
                    --name chr-mikrotik awg-mesh-chr-baseline:${CHR_VERSION}
      docker network connect --gw-priority -1 \
                             --driver-opt com.docker.network.endpoint.ifname=eth1 \
                             chr-e2e-net-<suffix> chr-mikrotik
      docker start chr-mikrotik
11. Verify the docker-routeros container still has `default ... dev eth0`;
    `eth1` is flushed and bridged into QEMU, so making the mesh network the
    default route breaks CHR host-forwarded SSH before RouterOS is reachable.
    Let Docker IPAM assign the `eth1` address; pinning a static secondary IP can
    leave RouterOS stuck during CHR boot on Docker Desktop/WSL.
12. Wait CHR SSH ready (~15s on a warm baseline).
13. Verify CHR reports `/system/device-mode container=yes` and the RouterOS
   `container` package is installed.
14. Verify CHR reaches the Linux control-plane through Docker gateway port publishing.
15. Upload `awg-mesh-client.tar` and the rewritten `deploy.rsc`.
16. ssh admin@chr '/import file-name=deploy.rsc verbose=yes'
17. Verify (HOST):
    - ssh admin@chr '/container/print where name=AWG_MESH_MIKROTIK_HOME' → status=running
18. Cleanup (or NO_CLEANUP=1 for inspection)
```

PASS criteria per runtime CHR version:

| Check | Layer |
|-------|-------|
| `mikrotik-chr-baseline-runtime.sh` passes first | bare RouterOS runtime |
| Probe container logs marker through `/log` | RouterOS container/logging |
| Probe traffic increases NAT and forward counters | RouterOS NAT/firewall |
| Generated `.rsc` accepted by `/import` (no syntax errors) | RouterOS CLI |
| `/container/print` shows `awg-mesh-client` running | RouterOS container |
| Generated `.rsc` contains no `/interface/wireguard` native configuration | release-scope guard |
| CHR reaches the Linux control-plane through Docker gateway port publishing | Docker/CHR network |

### 5.5 Runtime matrix dispatcher

`tests/simulation/mikrotik-version-matrix.sh` runs `mikrotik-chr-e2e.sh` for
each nftables-capable CHR runtime version sequentially (single `/dev/kvm` lane
locally; CI parallelizes across runners). The default runtime target is
`7.21.4`. It must not be used as evidence that pre-7.21 RouterOS releases can
run the current awg-mesh-client data plane; those releases are syntax targets
only.

### 5.6 Migration plan

| Sim | State | Replacement |
|-----|-------|-------------|
| `mikrotik-onboard.sh` (alpine proxy) | DEPRECATED | `mikrotik-chr-e2e.sh` |
| `mikrotik-chr-import.sh` (just `/import`, no container) | folded into | `mikrotik-chr-e2e.sh` extends — covers import + start + verify |

Removal: delete `mikrotik-onboard.sh` once the CHR matrix is stable in CI for
two release cycles.

---

## 6. Migration path for existing operators

Operators upgrading their MikroTik routers should:

1. **From 7.5–7.17 → 7.18+:** No action; `image=` syntax still works. `remote-image=` is the new canonical form but legacy `image=` is accepted.
2. **From any pre-7.21 → 7.21+:** Re-generate the deploy `.rsc` via `mesh-ctl node prepare --platform mikrotik --target-ros 7.21 <name>` (or omit the flag for the canonical default). Re-run `/import`. The old `.rsc` containing `mounts=` continues to work after 7.21 upgrade per MikroTik's compatibility note (*"old mount name becomes list name"*), but new mounts SHOULD use `mountlists=`.
3. **Downgrade** (7.21 → 7.20 or older): regenerate `.rsc` with `--target-ros 7.20` because `mountlists=` is unknown to pre-7.21 parsers.

A single `mesh-ctl` binary serves all three tiers; choice is per-deploy.

---

## 7. References

- [Container — MikroTik Documentation](https://help.mikrotik.com/docs/spaces/ROS/pages/84901929/Container) — canonical 7.21+ syntax reference.
- [Container — HomeAssistant — MikroTik Documentation](https://help.mikrotik.com/docs/spaces/ROS/pages/204341276/Container+-+HomeAssistant) — worked example.
- Per-version changelogs: `https://download.mikrotik.com/routeros/<version>/CHANGELOG` (replace `<version>`: `7.4`, `7.5`, `7.6`, `7.7`, `7.8`, `7.10`, `7.13`, `7.18`, `7.20`, `7.21`, `7.21.4`).
- [MikroTik download changelogs index](https://mikrotik.com/download/changelogs).
- Local runtime sim: `tests/simulation/mikrotik-version-matrix.sh` (default CHR version: `7.21.4`; override with `CHR_VERSIONS` for other `7.21+` / `7.23+` runtime targets).
- Local generator: `pkg/mikrotik/commands.go`, `pkg/mikrotik/templates.go`.

---

## 8. Outstanding follow-ups

Tracked under F-009 CR-014 and follow-up runtime hardening:

**Implemented in the v2.0 candidate:**

1. Add `TargetROSVersion` to `ContainerConfig` + `selectMikrotikDialect` helper (legacy / transitional / canonical).
2. `mesh-ctl node prepare --platform mikrotik --target-ros <version>` flag — default canonical, refuses pre-7.5 and 7.22.x.
3. Unit coverage for `7.16.2`, `7.20.8`, and `7.21.4` dialect selection.
4. Runtime CHR matrix driver covering nftables-capable RouterOS targets, defaulting to `7.21.4`.

**Remaining generator/operator docs:**

5. `mesh-topology.yml` nodes gain optional `target_ros: <version>` only if persistent per-node target selection becomes necessary.
6. Generate `INSTRUCTIONS.md` + `README-target-ros.md` alongside `.rsc` — operator-friendly steps for `/import` and verification.
7. Per-tier golden files (`testdata/deploy-golden-7.16.rsc`, `7.20.rsc`, `7.21.rsc`).

**Container runtime (self-healing):**

8. `awg-mesh-node` client init: probe `ip rule show`; if 7.22 regression detected (pref 1/2/3 hold local/main/default), normalise to `200`/`2147483646`/`2147483647`. Idempotent.
9. Probe nftables backend; fallback to iptables-legacy on `nft list ruleset` failure.
10. Probe `nf_nat` / conntrack / `/dev/net/tun`; emit startup feature-table for operator visibility.

**Test matrix (sim):**

11. Optional import-only CHR matrix for `7.16.2`, `7.20.8`, and `7.21.4` syntax pivots, kept distinct from the runtime/data-plane CHR gate.

**Documentation:**

12. README: document support matrix + 7.22 AVOID warning.
