# awg-mesh

## STACKS

```yaml
STACKS: [GO]
```

## PROJECT OVERVIEW

Docker-native encrypted overlay mesh network built on AmneziaWG.
Unified node binary (`awg-mesh-node`) + CLI control plane (`mesh-ctl`).

**Components:**
- `awg-mesh-node` — unified AWG node (master/endpoint/client modes)
- `mesh-ctl` — CLI for topology management, rotation, capture, onboarding

**Key features:**
- Topology-as-code (single YAML)
- Two-level ECMP load balancing with sticky sessions
- Health-checked failover
- AWG parameter rotation (anti-DPI)
- gRPC management with mTLS + token dual auth
- Prepare → deploy → init onboarding
- Endpoint key rotation propagates without container restart (v1.10+, via `UpdateTunnelPeer` RPC — fixes the split-brain scenario in `.agent/investigations/issue-92-endpoint-init-propagation.md`)
- `mesh-ctl master reload <name>` recovery primitive — force-reconciles admin-state pubkey to every bound endpoint on a named master
- `mesh-ctl inspect <node>` — 3-column drift report (Admin | Disk | Runtime) per peer; exit 1 on drift (v1.10.1+)
- `mesh-ctl reconcile` — idempotent topology-walk that force-syncs admin state to every node via gRPC (v1.10.1+)
- `mesh-ctl status --verify-data-plane` — L3 data-plane health probes per (master, endpoint) pair with structured failure reasons (v1.10.1+)
- `mesh-ctl upgrade <version>` — guided rolling upgrade with plan/confirm/execute/verify/rollback phases (v1.10.2+)
- `mesh-ctl upgrade compose <old-file>` — docker-compose schema migration helper for v1.5.1/v1.6.0/v1.9.0 → current (v1.10.2+)
- Endpoint per-master interface pattern (v1.12.2+): each bound master gets its own `wg-<master-name>` iface on the endpoint, avoiding WireGuard AllowedIPs dedup. Endpoint↔endpoint traffic flows via kernel policy routing. Symmetric with master-side architecture (local tracker #134).
- Master-side `MasterTunnel.AllowedIPs` admin source of truth (v1.12.9+): CLI computes and passes `AllowedIps` in every `AddTunnelRequest`; `saveTransportState` persists verbatim — no topology needed on master daemon. Eliminates `/27` loss on prod masters without `--topology` (local tracker #147 layers 3+4 — v1.12.8 added the field, v1.12.9 fixed the proto descriptor so wire marshal/unmarshal actually delivers it).
- Auto-insert `iptables -I FORWARD -i wg-+ -o wg-+ -j ACCEPT` on master startup (v1.12.10+): prevents silent DROP of endpoint↔endpoint overlay packets on Docker hosts with default DROP FORWARD policy (local tracker #150). Non-fatal: master logs a warning and continues if iptables is unavailable.

## ARCHITECTURE

```text
Control plane: mesh-ctl (Go CLI, admin PC)
  └── gRPC (mTLS + token) → awg-mesh-node instances

Data plane: awg-mesh-node (one container per host)
  ├── master mode: ingress + N tunnels + routing + healthcheck + capture
  ├── endpoint mode: AWG server + NAT + overlay IP
  └── client mode: tunnels to masters + overlay routing
```

## KEY FILES

- `mesh-topology.yml` — single source of truth for mesh state
- `.agent/specs/constitution.md` — non-negotiable project principles
- `.agent/specs/awg-mesh/` — spec, plan, tasks (in nvmd-devops repo, reference only)

## VERSIONING

Single version for the entire module (`go.mod` module path). Both binaries (`mesh-ctl`, `awg-mesh-node`) and the Docker image share the same version.

**Version detection:** two paths.
- `go install ...@v0.3.0` → `runtime/debug.ReadBuildInfo()` shows `v0.3.0`
- `go install ./cmd/mesh-ctl` (local clone) → `runtime/debug.ReadBuildInfo()` shows `v0.3.0 (abcd1234)` (base tag + commit)
- `go run` → `runtime/debug.ReadBuildInfo()` shows `dev`
- Docker builds → `main.versionFromBuild` is injected via ldflag from `--build-arg VERSION=...`

**When to bump:**
- **PATCH** (v0.3.0 → v0.3.1): bug fix, docs fix, test fix, dependency update, no behavior change
- **MINOR** (v0.3.0 → v0.4.0): new feature, new CLI command, new config field, new gRPC RPC
- **MAJOR** (v0.3.0 → v1.0.0): breaking change to CLI flags, topology YAML schema, gRPC API, or config directory layout. Requires explicit user approval.

**How to release:**
```bash
gh release create v0.X.Y --title "v0.X.Y" --notes "..."
git fetch --tags
```
Tag is created on GitHub, then fetched locally. No manual `git tag` needed.

**Rules:**
- Bump AFTER committing all changes for the release, not before
- Never move or delete existing tags
- Always include structured release notes with What's New / Changes / Fixes sections
- Docker image tags: `latest` (master), `v0.X.Y` (release), `<commit-sha>` (CI)
- `go install ./cmd/mesh-ctl` always shows the latest tag reachable from HEAD
- After `gh release create`: always `git fetch --tags` to sync local tags
- **Every release MUST have corresponding GitHub Container Registry (GHCR) AND Docker Hub image tags.** A release is incomplete until BOTH `docker manifest inspect ghcr.io/coonfuuseed-paandaa/awg-mesh-node:vX.Y.Z` AND `docker manifest inspect docker.io/coonfuuseedpaandaa/awg-mesh-node:vX.Y.Z` succeed (note: Docker Hub username has NO hyphen — `coonfuuseedpaandaa`, vs GHCR `coonfuuseed-paandaa`). If the pipeline failed to publish the semver tag to either registry (check `gh run list --workflow build.yml` for tag-triggered runs), dispatch the retag workflow manually: `gh workflow run build.yml -f retag_version=vX.Y.Z -f source_sha=<full-sha-from-main>`. The workflow publishes both registries in the same matrix job (see `.github/workflows/build.yml`, matrix entry `mirror_dockerhub: true`) — if one succeeded and the other failed it's a partial-publish incident and MUST be re-run. This is NON-NEGOTIABLE — a git tag without BOTH matching registry tags is a broken release.
- **Tag parity quick-check after any release:**
  - Docker Hub: `curl -s "https://hub.docker.com/v2/repositories/coonfuuseedpaandaa/awg-mesh-node/tags?page_size=100" | jq '[.results[].name | select(startswith("v"))] | sort | reverse'`
  - GHCR: `gh api users/coonfuuseed-paandaa/packages/container/awg-mesh-node/versions --jq '[.[] | .metadata.container.tags[]? | select(startswith("v"))] | unique | sort | reverse'`
  - Compare the two lists — any v-tag in one but missing from the other is a regression, re-dispatch retag for that version immediately.

## CONVENTIONS

- Go 1.25+, CGO enabled (gopacket/libpcap)
- amneziawg-go as library (import), not subprocess
- All management via gRPC, SSH only for bootstrap
- One binary, multiple modes (--mode master|endpoint|client)
- UAPI-first for runtime config changes

## RELEASE GATE — NON-NEGOTIABLE

**Docker smoke + e2e tests are MANDATORY before tagging any version.** Every
release (PATCH, MINOR, MAJOR) MUST pass `tests/simulation/issue-92-rotation.sh`
on a real WSL2/Linux host with Docker. `go test -short ./...` and CI build
green are NOT sufficient — they validated v1.12.0 which then failed e2e
catastrophically (broken tier-3 rotation: idempotency check + master
applyPeerKeyUpdate device-handle drift).

**Process for every release:**
1. `go test -race -count=1 ./...` — all packages green (race detector mandatory; v1.14.0 tag-build caught a real overlayRouterFn race that re-runs masked)
2. `docker build -t awg-mesh-node:local -f deploy/Dockerfile.node .`
3. `bash tests/simulation/issue-92-rotation.sh` — MUST exit 0 with all R1-R12 PASS (includes R3a-R3g, R9 persistence gate, R10 route-get src assertions and endpoint↔endpoint ping matrix, R11 master AllowedIPs endpoints-range gate, R11b no-topology master persists /27, R12 master FORWARD ACCEPT gate)
4. `bash tests/simulation/mikrotik-chr-e2e.sh CHR=7.16.2` — MUST PASS 10/10 on real RouterOS CHR (operator-side QEMU/KVM coverage; replaces chr-lint CI gate which is impossible without nested-virt runner)
5. G3 unit tests green: `go test -run 'TestReadEndpointPublicKeyFormats|TestReadAdminPubkeyRawFormats' ./...`
6. G7 unit tests green: `go test -run 'TestPortOffset|TestComputePeerEndpoint' ./...`
7. G14 wire gate green: `go test -run 'TestAddTunnelRequest_AllowedIpsWireRoundtrip' ./proto/...`
8. Golden-fixture diff green: `go test ./pkg/mikrotik/ -run TestGolden` (compile-time RouterOS .rsc generator regression catch)
9. ONLY THEN: tag, gh release create, verify GHCR + Docker Hub parity

> **F-004 extended data-plane gate (DEFERRED — not yet wired):** `tests/simulation/data-plane-extended.sh` orchestrates 6 FR modules covering ECMP balance / iperf3 throughput / conntrack sticky / failover timing / asymmetric routing / sticky migration. SHIPPED in v1.15.0 candidate but **NOT yet included in this release gate** — pending F-005 (fixture client-mode container addition) and TD-2026-04-30-F-004-INTEGRATION-FINDINGS resolution. See `.agent/specs/F-004-extended-data-plane-tests/changes/CR-003-fixture-client-mode-prerequisite/change.md` and CHANGELOG `[Unreleased]` "Known limitations" for the conditionality.

**If e2e fails:** investigate root cause, fix, re-run sim — do NOT ship.
"Tests pass + lint clean" without e2e proves only that the code compiles,
not that it works.

This rule was added 2026-04-19 after v1.12.0 shipped broken because e2e
was skipped. v1.12.0 had to be reverted.

## LOCAL TOOLING — github-panda (NOT committed)

Repo uses `coonfuuseed-paandaa` GitHub account, but default `gh auth` for
multi-repo developers is often a different identity (e.g. `thebtf` for the
maintainer). Using the wrong identity leaks account linkage via PR/issue
comments.

`tools/github-panda/` (gitignored — not in repo) wraps `gh` / `git push` /
`curl` with the correct PAT inline:

| File | Shell |
|------|-------|
| `tools/github-panda/github-panda.sh` | bash / WSL / git-bash |
| `tools/github-panda/github-panda.ps1` | Windows PowerShell |

The tool is self-bootstrapping: subcommands include `whoami`, `gh <args>`,
`api <path>`, `pr <subcmd>`, `push [<branch>]`, `curl <url>`, `env`. Read
the script header for full usage.

**Why .gitignored:** the wrapper hardcodes the PAT for ergonomics. Putting it
in git would publish the token. The directory entry `tools/github-panda/`
is in `.gitignore` (line 52). `git status` periodically to verify drift hasn't
re-added it.

**How to install on a new machine:**

1. Create directory `tools/github-panda/` in this repo (it's gitignored).
2. Copy the latest `github-panda.sh` + `github-panda.ps1` from the previous
   machine via rsync / scp / USB / encrypted transfer (NOT through this git
   repo — they're not in it).
3. Optional alias: `alias gp='bash tools/github-panda/github-panda.sh'`
   (bash) or `function gp { & "$PWD\tools\github-panda\github-panda.ps1" @args }`
   (PowerShell `$PROFILE`).
4. Verify: `gp whoami` — must print `coonfuuseed-paandaa`.

**Token rotation:** update both `.sh` and `.ps1` inline, then `gp whoami` to
verify. Old token revoked on github.com/settings/tokens.

**Cross-platform note:** PowerShell does not support bash-style env-prefix
syntax (`GH_TOKEN=foo gh ...`). The wrapper handles this by setting the env
var inside the script and removing it on exit. Use the matching wrapper for
your shell.
