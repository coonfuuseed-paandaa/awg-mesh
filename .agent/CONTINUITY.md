# Continuity State

**Last Updated:** 2026-03-26
**Session:** Project initialization — spec, plan, tasks, constitution, analyze

## Done This Session

- Designed full AWG mesh architecture through iterative brainstorming
- Created spec.md: 14 FR, 6 NFR, 7 user stories, 8 edge cases
- Ran /speckit-clarify: resolved 4 open questions (key storage, token model, thin mode, metrics)
- Created plan.md: 6 phases, 12 Go libraries, architecture, file structure
- Verified amneziawg-go: N devices per process confirmed (no patches needed)
- Verified Jipok/wgctrl-go: all AWG params supported (Jc/Jmin/Jmax, S1-S4, H1-H4, I1-I5)
- Created tasks.md: 82 tasks across 6 phases
- Ran /speckit-analyze: 14 findings (0 CRITICAL, 4 HIGH)
- Fixed all 4 HIGH findings (stale references from pre-clarification decisions)
- Created constitution.md: 7 non-negotiable principles (C1-C7)
- Created architecture checklist: 65 items
- Initialized repo at D:\Dev\awg-mesh, pushed to github.com/thebtf/awg-mesh (private)
- Studied amneziawg-scripts research: UAPI rotation, DPI fingerprinting, rotation consistency

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

## Design Docs

```
.agent/data/
  mesh-router-design.md    — core architecture, labels, load balancing, terminology
  mesh-processes.md        — operational processes, rotation, capture, gRPC, commands
  mesh-onboarding.md       — prepare → deploy → init protocol
  mesh-unified-node.md     — single container per host design
  mesh-address-space.md    — named ranges, CIDR notation, MTU, clamp-to-PMTU
  mesh-overlay-design.md   — overlay network concept (early draft)
```

## Analyze Report (2026-03-26)

| ID | Sev | Status | Summary |
|----|-----|--------|---------|
| A1 | HIGH | FIXED | FR-10 "except thin mode" removed |
| A2 | HIGH | FIXED | NFR-2 updated to MESH_TOKEN dual auth |
| A3 | HIGH | FIXED | FR-9 token remains as fallback, not invalidated |
| A4 | MEDIUM | FIXED | FR-12 MikroTik full gRPC/UAPI support |
| C1 | HIGH | FIXED | Bootstrap task T006a added |
| C2 | MEDIUM | FIXED | Reconciliation task T016a added |
| E1 | MEDIUM | FIXED | INIT_TOKEN → MESH_TOKEN everywhere |
| B1-F2 | LOW-MED | NOTED | Non-blocking, documented in checklist |

## Dependencies (verified)

| Library | Status |
|---------|--------|
| amneziawg-go | VERIFIED: N devices per process, no patches |
| Jipok/wgctrl-go | VERIFIED: all AWG params in Config struct |
| MikroTik container gRPC | UNVERIFIED: needs Phase 5 test |
| gopacket on Alpine | UNVERIFIED: needs Phase 4 test |

## Next

1. Phase 0 tasks: T001 (done: repo created), T002-T006a
2. Phase 1: AWG interface management PoC
3. Start in `D:\Dev\awg-mesh` — run `/load` then `/autopilot` or manual phase execution
