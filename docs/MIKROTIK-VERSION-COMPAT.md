# MikroTik RouterOS Version Compatibility Playbook

**Audience:** awg-mesh `mesh-ctl` developers + operators choosing target RouterOS for MikroTik clients.
**Goal:** define which RouterOS 7.x versions awg-mesh supports, what syntax pivots happened in `/container/*` and `/ip/firewall/*`, and how the `.rsc` generator (`pkg/mikrotik`) MUST adapt to keep client containers bootable across the full v7 series.

All facts are verified against MikroTik's official changelogs at `https://download.mikrotik.com/routeros/<version>/CHANGELOG`. Inline citations point to the exact changelog entry.

---

## 1. Supported version range

| Range | Support tier | Why |
|-------|--------------|-----|
| **< 7.4** | NOT SUPPORTED | `/container` package does not exist. v6 is dead-end. |
| **7.4 (testing)** | NOT SUPPORTED | Container available only in `testing` channel (per 7.4 CHANGELOG: *"Container package is not available in v7.4. Development and testing continues in 'testing' channel."*). Containers must be recreated against 7.5 anyway. |
| **7.5 — 7.17** | **TIER 2 (legacy syntax)** | First stable container release (per 7.5 CHANGELOG: *"added support for running Docker (TM) containers on ARM, ARM64 and x86 (containers created before v7.4 must be recreated)"*). Pre-`remote-image` and pre-`mountlists` dialect. Generator MUST emit legacy form. |
| **7.18 — 7.20** | **TIER 1.5 (transitional)** | `remote-image=` introduced (7.18 CHANGELOG: *"allow specifying registry using remote-image property"*). Mount reference parameter still called `mounts=`. `envlist=` accepts multiple values starting 7.20. Generator MAY emit either dialect. |
| **7.21+** (LTS) | **TIER 1 (canonical)** | Mount reference parameter renamed `mounts=` → `mountlists=` (7.21 CHANGELOG: *"convert container mounts setting to mountlists, old mount name becomes list name, list name can map to multiple mounts"*). `/app` menu introduced. **awg-mesh canonical target.** |
| **7.22.x** | **AVOID — known regression** | Default `ip rule` priorities inside containers regressed to consecutive low (1/2/3), breaking DSCP policy routing for awg-mesh client. See §3.3. Reverted in 7.23rc2. |
| **7.23+** | TIER 1 (canonical, regression-free) | Same dialect as 7.21 (mountlists/list/remote-image). `ip rule` priorities restored. |

**Decision:** `mesh-ctl` MUST be able to generate `.rsc` for 7.5 through 7.21+. The default target is the latest LTS at release time (currently 7.21.x).

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

**Confirmed AVAILABLE in RouterOS containers (per MikroTik forum operator reports + our v1.14.0 runtime testing):**

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

**Two-layer remediation** (per project policy "self-heal in container, не раздуваем флаги"):

1. **Generator (offline):** refuses `--target-ros 7.22.x` with operator-friendly error pointing to 7.21 LTS or 7.23+. No silent generation against a known-broken target.
2. **Container runtime:** `awg-mesh-node` client mode probes `ip rule show` at startup. If `pref 1` / `pref 2` / `pref 3` are detected on `local`/`main`/`default` (signature of 7.22 regression), the runtime normalises them to `200` / `2147483646` / `2147483647` before installing DSCP rules. Idempotent, non-fatal on `ENOENT`. Logs a warning naming the regression.

This means: if an operator targets 7.21 (default) and later upgrades the router to 7.22, the container self-heals at next start without `.rsc` regeneration. If the operator explicitly targets 7.22 in the generator, they get a clear refusal — pointing them at 7.21 or 7.23.

(Earlier playbook v1 dismissed all nftables concerns. That was wrong for the container side. Section 3.1 still holds for host CLI; sections 3.2 + 3.3 are the load-bearing parts for client containers running under RouterOS.)

---

## 4. Architecture — two layers, no auto-deploy

awg-mesh splits MikroTik compatibility across two layers. Generator stays offline + operator-driven; container handles runtime kernel quirks autonomously. **No auto-deploy** — operator configurations differ too much (CRS vs hAP vs CHR, custom firewall, VLAN trunks, IPv6 layouts) to safely SSH and apply changes.

### 4.1 Generator (offline, operator-driven)

`mesh-ctl client deploy <name>` produces a deploy bundle on disk. Operator copies + imports manually following generated `INSTRUCTIONS.md`.

**Bundle layout:**
```
<work-dir>/clients/<name>/
  <name>-mikrotik.rsc        # canonical .rsc for the chosen tier
  INSTRUCTIONS.md            # step-by-step for operator (verify version, /import, troubleshoot)
  README-target-ros.md       # which tier was used, why, downgrade/upgrade notes
```

**Configuration sources (priority — highest wins):**

1. CLI flag `--target-ros 7.16` (per-invocation override)
2. `mesh-topology.yml` client field `target_ros: 7.20` (per-client persistent default)
3. Built-in default: latest LTS at release time (currently `7.21`)

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

```
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

### 4.2 Container runtime (self-healing inside `awg-mesh-node`)

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
```
client-init: kernel features:
  nftables=yes  iptables-legacy=yes  nf_nat=yes  conntrack=yes  tun=yes
  ip-rule-regression=detected-and-fixed (was pref 1/2/3, now 200/big-1/big)
```

### 4.3 Why no auto-deploy

Operators run RouterOS in heterogeneous setups: hAP/CRS hardware, CHR on KVM/VMware/Proxmox, custom firewall layers, VLAN trunks, MPLS, dual-WAN failover, IPv6-only management. SSH-and-apply cannot anticipate enough corner cases to be safe. We give operator a deterministic `.rsc` + `INSTRUCTIONS.md`; they decide where it fits in their config.

---

## 5. Test matrix (sim)

`tests/simulation/mikrotik-chr-import.sh` MUST be parameterized to run against ≥3 CHR versions covering each tier:

| Tier | CHR tag | Why included |
|------|---------|--------------|
| TIER 1 (canonical) | `7.21.4` | Latest LTS, default target |
| TIER 1.5 (transitional) | `7.20.8` | Last 7.20 patch — verifies `remote-image=` accepted, `mounts=` still works |
| TIER 2 (legacy) | `7.16.2` | Common deployed version pre-`remote-image` — verifies legacy dialect |

The test script accepts `CHR_VERSION` env var; CI matrix dispatches three jobs in parallel (sequential locally if `/dev/kvm` is single-threaded). PASS criterion per version:
- I.1 `.rsc` upload succeeds (scp).
- I.2 `/import` exits 0.
- I.3 No `Script Error: syntax error` in `/import` output.
- I.4 No regression of Bug 1 ordering (Veth before Bridge).
- I.5 Canonical params accepted (3a/3b/5).
- V.1 `AWG_MESH_*` interfaces present after import.

**Failure mode that this matrix catches** (already observed against `7.16.2` in v1.14.0-pre dev run):
```
/container/mounts/add list=AWG_MESH_MIKROTIK_HOME_CONFIG src=...
Script Error: syntax error (line 42 column 11)
```
With version-aware generator producing `name=` instead of `list=` for the legacy tier, that line becomes `/container/mounts/add name=...` and imports cleanly.

---

## 6. Migration path for existing operators

Operators upgrading their MikroTik routers should:

1. **From 7.5–7.17 → 7.18+:** No action; `image=` syntax still works. `remote-image=` is the new canonical form but legacy `image=` is accepted.
2. **From any pre-7.21 → 7.21+:** Re-generate the deploy `.rsc` via `mesh-ctl client deploy --target-ros 7.21` (or omit flag — it's the default). Re-run `/import`. The old `.rsc` containing `mounts=` continues to work after 7.21 upgrade per MikroTik's compatibility note (*"old mount name becomes list name"*), but new mounts SHOULD use `mountlists=`.
3. **Downgrade** (7.21 → 7.20 or older): regenerate `.rsc` with `--target-ros 7.20` because `mountlists=` is unknown to pre-7.21 parsers.

A single `mesh-ctl` binary serves all three tiers; choice is per-deploy.

---

## 7. References

- [Container — MikroTik Documentation](https://help.mikrotik.com/docs/spaces/ROS/pages/84901929/Container) — canonical 7.21+ syntax reference.
- [Container — HomeAssistant — MikroTik Documentation](https://help.mikrotik.com/docs/spaces/ROS/pages/204341276/Container+-+HomeAssistant) — worked example.
- Per-version changelogs: `https://download.mikrotik.com/routeros/<version>/CHANGELOG` (replace `<version>`: `7.4`, `7.5`, `7.6`, `7.7`, `7.8`, `7.10`, `7.13`, `7.18`, `7.20`, `7.21`, `7.21.4`).
- [MikroTik download changelogs index](https://mikrotik.com/download/changelogs).
- Local sim: `tests/simulation/mikrotik-chr-import.sh` (CHR_VERSION env var).
- Local generator: `pkg/mikrotik/commands.go`, `pkg/mikrotik/templates.go`.

---

## 8. Outstanding follow-ups

Tracked under F-001 CR-002 (`.agent/specs/mikrotik-generator-fixes/changes/CR-002-multi-version-support/change.md`):

**Generator (offline):**

1. Add `TargetROSVersion` to `ContainerConfig` + `selectMikrotikDialect` helper (legacy / transitional / canonical).
2. `mesh-ctl client deploy --target-ros <version>` flag — default latest LTS, refuses pre-7.5 and 7.22.x.
3. `mesh-topology.yml` clients gain optional `target_ros: <version>` (CLI flag overrides per-invocation).
4. Generate `INSTRUCTIONS.md` + `README-target-ros.md` alongside `.rsc` — operator-friendly steps for `/import` and verification.
5. Per-tier golden files (`testdata/deploy-golden-7.16.rsc`, `7.20.rsc`, `7.21.rsc`).

**Container runtime (self-healing):**

6. `awg-mesh-node` client init: probe `ip rule show`; if 7.22 regression detected (pref 1/2/3 hold local/main/default), normalise to `200`/`2147483646`/`2147483647`. Idempotent.
7. Probe nftables backend; fallback to iptables-legacy on `nft list ruleset` failure.
8. Probe `nf_nat` / conntrack / `/dev/net/tun`; emit startup feature-table for operator visibility.

**Test matrix (sim):**

9. Parameterize `mikrotik-chr-import.sh` over CHR_VERSION; add `mikrotik-version-matrix.sh` driver covering CHR 7.16.2 / 7.20.8 / 7.21.4.
10. CI: multi-tier CHR job invoking matrix on every PR.
11. Resolve CHR 7.21.4 first-boot SSH password handshake — current script handles 7.16 empty-pass but not 7.21.4 (empirical 2026-04-29).

**Documentation:**

12. README + CHANGELOG: document support matrix + 7.22 AVOID warning + container self-heal behaviour.
