# Continuity State

**Last Updated:** 2026-03-27
**Session:** Autopilot — Phases 0-2 complete, Phase 3 in progress

## Done This Session

- Phase 0 complete (PR #1): skeleton, Makefile, Dockerfile, CI, topology example
- Phase 1 complete (PR #2-#4): pkg/wg (own UAPI client, no wgctrl-go), pkg/topology, pkg/node, proto, integration test skeleton
- Phase 2 complete (PR #5-#6): pkg/tls (mTLS CA ECDSA P-256), pkg/grpc (dual auth), mesh-ctl (cobra + all commands)
- Phase 3 delegated to codex — may or may not have completed before session end

## Tasks Status (35/82 done)

- Phase 0: T001-T006 DONE (T006a bootstrap deferred)
- Phase 1: T007-T017 DONE
- Phase 2: T018-T035 DONE (T036 E2E test deferred)
- Phase 3: T037-T043 IN PROGRESS (codex delegation — check if files exist on disk)
- Phase 3: T044-T047 NOT STARTED (lifecycle commands, templates, integration test)
- Phase 4-6: NOT STARTED

## PRs

- PR #1-#6 merged to master (squash)
- Worktree: D:\Dev\awg-mesh\.claude\worktrees\phase-1-awg (branch worktree-phase-1-awg)

## Key Design Decisions

1. Unified node binary — one binary, three modes (master/endpoint/client)
2. No wgctrl-go dependency — own UAPI client in pkg/wg/uapi.go
3. amneziawg-go v1.0.4 as library
4. ECDSA P-256 for CA (not RSA)
5. Dual auth: mTLS primary + MESH_TOKEN (bcrypt) fallback
6. Topology as code — mesh-topology.yml
7. Docker-compose templates rendered by mesh-ctl prepare
8. gRPC on :9090, metrics on :9091

## Compact Prompt for Next Session

```
/load
/autopilot

Phase 0-2 done (6 PRs merged, 35 tasks). Phase 3 in progress.
Check if pkg/routing/ and pkg/node/master.go exist in worktree.
If yes: verify build, commit, PR, continue with T044-T047.
If no: re-delegate Phase 3 (T037-T043) to codex.
Then: Phase 4 (awggen), Phase 5 (MikroTik), Phase 6 (observability).
Worktree: D:\Dev\awg-mesh\.claude\worktrees\phase-1-awg
```
