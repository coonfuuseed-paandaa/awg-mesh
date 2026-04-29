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

## 3. nftables question — **resolved: NO user-facing change**

RouterOS v7 ships a kernel-level `nf_tables` backend used internally by `/ip/firewall`. **No CHANGELOG entry mentions a user-facing CLI rewrite of `/ip/firewall`** between 7.4 and 7.21 — the table/chain/action grammar is stable across the entire 7.x series.

What did change inside the engine (transparent to operators):
- 7.18: postrouting chain gained `in-interface`, `in-bridge-port`, `in-bridge` matchers. Earlier versions reject those keys in srcnat/postrouting.
- All versions accept `action=fasttrack-connection` and `action=accept`/`drop` with identical semantics.

**Generator implications:**
- `awg-mesh` does NOT use postrouting in-bridge matchers, so the 7.18 expansion is irrelevant.
- The fasttrack anchor logic in `pkg/mikrotik/commands.go` (`/ip/firewall/filter find where action=fasttrack-connection chain=forward` then `place-before=$fastTrackId`) works identically on 7.5 — 7.21+.
- **No version branching needed for firewall.** A single template covers the whole supported range.

(Earlier internal speculation about an "nftables migration" affecting our config was wrong. The kernel backend is irrelevant; the CLI surface — which is what `.rsc` interacts with — never changed.)

---

## 4. Generator architecture (target)

`pkg/mikrotik/commands.go::ContainerConfig` MUST gain a `TargetROSVersion` field:

```go
type ContainerConfig struct {
    // ... existing fields ...

    // TargetROSVersion is the minimum RouterOS version the generated .rsc must
    // import cleanly on. Default: 7.21.0. Accepted: 7.5.0+.
    // Drives dialect selection: legacy (7.5-7.17), transitional (7.18-7.20),
    // canonical (7.21+).
    TargetROSVersion string
}
```

Helper: `selectMikrotikDialect(version string) Dialect` returns one of three constants. Functions `GenerateContainerCommands` / `GenerateMountCommand` / `GenerateImagePart` consult the dialect.

Hand-off rules for each pivot:

```
imagePart(cfg) =
    if dialect == LEGACY:        "image="          + cfg.Image
    if dialect == TRANSITIONAL:  "remote-image="   + cfg.Image     // also accepts image=
    if dialect == CANONICAL:     "remote-image="   + cfg.Image

mountListRef(cfg) =
    if dialect == CANONICAL:    "mountlists=" + cfg.MountName
    else:                       "mounts="     + cfg.MountName

mountCreateLine(cfg) =
    if dialect == CANONICAL:   "/container/mounts/add list=NAME src=... dst=..."
    else:                      "/container/mounts/add name=NAME src=... dst=..."
```

Default for `mesh-ctl` CLI: `--target-ros 7.21` (canonical). Operators on legacy hardware override via `--target-ros 7.16` (TIER 2) or `--target-ros 7.18` (TIER 1.5).

`mesh-topology.yml` clients gain optional `target_ros: <semver>` per-client; CLI flag overrides per-invocation.

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

Tracked under feature `mikrotik-version-compat` (CR-002 of F-001 once spec amend lands):

1. Add `TargetROSVersion` to `ContainerConfig` + `selectMikrotikDialect` helper.
2. Extend `mesh-ctl client deploy` with `--target-ros` flag (default `7.21`).
3. Extend `mesh-topology.yml` schema with optional `target_ros` per-client.
4. Parameterize `mikrotik-chr-import.sh` matrix run; add `tests/simulation/mikrotik-version-matrix.sh` driver.
5. CI: add multi-tier CHR job (3 versions × .rsc generation × /import).
6. README + CHANGELOG: document the support matrix.
7. Resolve CHR 7.21.4 first-boot SSH password handshake (current `mikrotik-chr-import.sh` SSHs in on `7.16` cleanly but 7.21.4 rejects empty password — empirical finding 2026-04-29).
