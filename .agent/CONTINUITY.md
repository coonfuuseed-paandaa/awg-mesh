# Continuity State

**Last Updated:** 2026-03-27
**Session:** Phase 0 implementation + PR review

## Done This Session

- Implemented Phase 0 (T001-T006) via codex delegation
- cmd/awg-mesh-node/main.go: --mode flag dispatch (master/endpoint/client)
- cmd/mesh-ctl/main.go: cobra root + version subcommand
- Makefile: build/test/lint/proto-gen/docker/clean
- deploy/Dockerfile: multi-stage golang:1.24-alpine → alpine:3.21
- mesh-topology.example.yml: full schema with realistic values
- .github/workflows/build.yml: lint + test + build + GHCR push
- Fixed .gitignore binary patterns (was excluding cmd/ subdirs)
- Fixed CI branch refs (main → master)
- Fixed Dockerfile version injection (ldflags)
- Fixed Makefile docker target (-f deploy/Dockerfile)
- Fixed topology preset (paranoid → aggressive per spec)
- go build ./... passes clean
- Created PR #1, pushed to worktree-phase-0-init branch
- Launched PR review (CodeRabbit + Gemini + Codex)

## Done Previous Session

- Full spec/plan/tasks/constitution/analyze cycle (see below)

## Key Design Decisions

1. **Unified node binary** — one binary (awg-mesh-node), three modes (master/endpoint/client)
2. **One container per host** — all tunnels + routing inside one container, no Docker-in-Docker
3. **Independent masters** — no inter-master communication, MikroTik ECMP for failover (C2)
4. **Topology as code** — mesh-topology.yml, single source of truth (C3)
5. **UAPI-first** — zero downtime rotation via amneziawg-go device.IpcSet() (C4)
6. **Dual auth** — mTLS primary + MESH_TOKEN permanent fallback, rotatable (C5)
7. **No external deps** — no etcd, no Consul, no cloud services (C6)
8. **Go-only** — single language, amneziawg-go as library (C7)
9. **Prepare → deploy → init** — 3-step onboarding, token for auth
10. **Two-level ECMP** — MikroTik across masters + master across endpoints
11. **Named CIDR ranges** — balancer IP any address, auto-allocation
12. **AWG param generation** — Go port of awg_gen.py, capture-based, per-master unique
13. **gRPC management** — all ops via gRPC, SSH only for bootstrap

## Spec Artifacts

```
.agent/specs/awg-mesh/
  spec.md              — 14 FR, 6 NFR, 7 user stories (Clarified)
  plan.md              — 6 phases, architecture, libraries
  tasks.md             — 82 tasks
  checklists/
    architecture.md    — 65 quality checks
.agent/specs/
  constitution.md      — 7 principles (C1-C7)
```

## Working Set

- Worktree: D:\Dev\awg-mesh\.claude\worktrees\phase-0-init (branch: worktree-phase-0-init)
- PR: https://github.com/thebtf/awg-mesh/pull/1
- PR review: CodeRabbit + Gemini + Codex invoked, awaiting results

## Dependencies (verified)

| Library | Status |
|---------|--------|
| amneziawg-go | VERIFIED: N devices per process, no patches |
| Jipok/wgctrl-go | VERIFIED: all AWG params in Config struct |
| spf13/cobra | VERIFIED: in go.mod, build passes |
| MikroTik container gRPC | UNVERIFIED: needs Phase 5 test |
| gopacket on Alpine | UNVERIFIED: needs Phase 4 test |

## Next

1. Wait for PR #1 review results, address findings, merge
2. T006a: mesh-ctl bootstrap --host (SSH Docker install) — may skip for now
3. Phase 1: AWG interface management PoC (T007-T017)
4. Testing: Docker Desktop available locally
