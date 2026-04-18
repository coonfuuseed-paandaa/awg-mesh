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

## ARCHITECTURE

```
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

## CONVENTIONS

- Go 1.25+, CGO enabled (gopacket/libpcap)
- amneziawg-go as library (import), not subprocess
- All management via gRPC, SSH only for bootstrap
- One binary, multiple modes (--mode master|endpoint|client)
- UAPI-first for runtime config changes
