# Tasks: AWG Mesh

**Spec:** .agent/specs/awg-mesh/spec.md
**Plan:** .agent/specs/awg-mesh/plan.md
**Generated:** 2026-03-26

## Phase 0: Project Init

- [x] T001 Create repository D:\Dev\awg-mesh with Go module, CLAUDE.md, AGENTS.md, .agent/ in cmd/awg-mesh-node/main.go
- [x] T002 [P] Create Makefile with targets: build, test, proto-gen, docker, lint in Makefile
- [x] T003 [P] Create Dockerfile (multi-stage: Go build + Alpine runtime) in deploy/Dockerfile
- [x] T004 Create mesh-topology.example.yml with full schema example in mesh-topology.example.yml
- [x] T005 [P] Set up GitHub Actions build + push to GHCR in .github/workflows/build.yml
- [x] T006 Add amneziawg-go and Jipok/wgctrl-go as Go dependencies in go.mod
- [ ] T006a [P] Implement `mesh-ctl bootstrap --host IP` — SSH Docker install, pull image in cmd/mesh-ctl/cmd/bootstrap.go

## Phase 1: AWG Interface Management (FR-1 partial, FR-2)

**Goal:** Create and manage AWG interfaces programmatically. Single tunnel proof-of-concept.
**Test criteria:** Two awg-mesh-node processes establish a tunnel and ping each other.

- [x] T007 [US1] Implement TUN device creation + device.NewDevice() wrapper in pkg/wg/interface.go
- [x] T008 [US1] Implement UAPI param set/get via device.IpcSet() in pkg/wg/uapi.go
- [x] T009 [P] [US1] Implement WG key generation (Curve25519) in pkg/wg/keygen.go
- [x] T010 [P] [US1] Implement AWG conf file generation from Go structs in pkg/wg/config.go
- [x] T011 [US1] Implement topology.yml parser with CIDR range support in pkg/topology/topology.go
- [x] T012 [P] [US1] Implement named range management + IP allocator in pkg/topology/ranges.go
- [x] T013 [P] [US1] Implement topology validation (overlap, orphan, CIDR) in pkg/topology/validate.go
- [x] T014 [US1] Implement awg-mesh-node skeleton with --mode flag dispatch in cmd/awg-mesh-node/main.go
- [x] T015 [US1] Implement endpoint mode: single AWG server interface + NAT + overlay IP in pkg/node/endpoint.go
- [x] T016 [US1] Implement client mode: single AWG client interface in pkg/node/client.go
- [x] T016a Implement config persistence + startup reconciliation (load saved config → restore interfaces on restart) in pkg/node/config.go
- [x] T017 Verify: two awg-mesh-node containers establish tunnel + ping overlay IP (integration test)

## Phase 2: gRPC + Auth (FR-9, FR-10)

**Goal:** gRPC server with dual auth, mesh-ctl init workflow.
**Test criteria:** mesh-ctl prepare → deploy → init completes, mTLS active.

- [x] T018 Define protobuf service AwgAgent (Init, AddPeer, RemovePeer, RotateParams, Status) in proto/agent.proto
- [x] T019 [P] Define protobuf shared types (AwgParams, NodeStatus, TunnelStatus) in proto/types.proto
- [x] T020 Generate Go code from protobuf in Makefile (proto-gen target)
- [x] T021 Implement mTLS CA generation + cert issuance in pkg/tls/ca.go
- [x] T022 [P] Implement cert load/save/validate utilities in pkg/tls/cert.go
- [x] T023 [P] Implement MESH_TOKEN generation + hashing + rotation in pkg/tls/token.go
- [x] T024 Implement gRPC server with dual auth interceptor (mTLS primary, token fallback) in pkg/grpc/server.go
- [x] T025 [P] Implement gRPC client with auto-fallback (try mTLS → token) in pkg/grpc/client.go
- [x] T026 Implement Init RPC handler: receive certs, save config, transition to mTLS in pkg/grpc/handlers.go
- [x] T027 Implement mesh-ctl cobra skeleton with root + version commands in cmd/mesh-ctl/main.go
- [x] T028 Implement `mesh-ctl endpoint prepare` — generate docker-compose + token in cmd/mesh-ctl/cmd/endpoint.go
- [x] T029 [P] Implement `mesh-ctl master prepare` — generate docker-compose + token in cmd/mesh-ctl/cmd/master.go
- [x] T030 [P] Implement `mesh-ctl client prepare` — generate config for linux/mikrotik in cmd/mesh-ctl/cmd/client.go
- [x] T031 Implement `mesh-ctl endpoint init` — connect via token, exchange certs, configure node in cmd/mesh-ctl/cmd/endpoint.go
- [x] T032 Implement `mesh-ctl master init` in cmd/mesh-ctl/cmd/master.go
- [x] T033 Implement `mesh-ctl client init` in cmd/mesh-ctl/cmd/client.go
- [x] T034 Implement `mesh-ctl token rotate --node X` in cmd/mesh-ctl/cmd/token.go
- [x] T035 Implement `mesh-ctl status` — query all nodes via gRPC in cmd/mesh-ctl/cmd/status.go
- [ ] T036 Verify: full prepare → deploy → init → mTLS active → status works (integration test)

## Phase 3: Master Mode (FR-1, FR-4, FR-5, FR-13)

**Goal:** Master node with multi-tunnel, routing, ECMP, healthcheck, auto MTU.
**Test criteria:** Master routes traffic through overlay to endpoint, failover works.

- [ ] T037 [US2] Implement master mode: multi-interface management via AddTunnel/RemoveTunnel gRPC in pkg/node/master.go
- [ ] T038 [US2] Implement Linux route management (ip route add/del/replace) in pkg/routing/route.go
- [ ] T039 [P] [US2] Implement ECMP multipath routes for balancer IPs in pkg/routing/ecmp.go
- [ ] T040 [P] [US2] Implement iptables NAT rules (MASQUERADE) in pkg/routing/nat.go
- [ ] T041 [P] [US2] Implement TCP MSS clamping (--clamp-mss-to-pmtu) in pkg/routing/mss.go
- [ ] T042 [US4] Implement healthcheck loop: ping overlay IPs, auto-remove/re-add routes in pkg/node/health.go
- [ ] T043 [US2] Implement auto MTU calculation based on hop count in pkg/node/mtu.go
- [ ] T044 [US1] Implement `mesh-ctl endpoint prepare/init/remove` full lifecycle calling master AddTunnel in cmd/mesh-ctl/cmd/endpoint.go
- [ ] T045 [US2] Implement `mesh-ctl master prepare/init/remove` full lifecycle with all endpoints in cmd/mesh-ctl/cmd/master.go
- [ ] T046 [P] Implement docker-compose templates for master and endpoint in deploy/templates/
- [ ] T047 Verify: master ↔ endpoint tunnel, overlay route, ping, failover on endpoint kill (integration test)

## Phase 4: AWG Param Generation + Rotation (FR-6, FR-7, FR-8, FR-14)

**Goal:** Port awg_gen.py to Go, implement all rotation tiers.
**Test criteria:** Tier 1 rotation with zero packet loss, capture from live domains.

- [ ] T048 [US3] Port protocol families (9 types) from awg_gen.py to Go in pkg/awggen/families.go
- [ ] T049 [P] [US3] Port preset ranges (aggressive/balanced/minimal) in pkg/awggen/presets.go
- [ ] T050 [P] [US3] Port I-spec tag encoding (<b>, <r>, <rc>, <rd>, <t>) in pkg/awggen/ispec.go
- [ ] T051 [US3] Port param generator (S1-S4, H1-H4, Jc/Jmin/Jmax, I1-I5 from captures) in pkg/awggen/generator.go
- [ ] T052 [P] [US3] Port MTU validation + S3/S4 constraint checking in pkg/awggen/mtu.go
- [ ] T053 [US7] Implement TLS/QUIC packet capture via gopacket in pkg/awggen/capture.go
- [ ] T054 [US3] Implement Tier 1 rotation protocol (UAPI SET on both sides) in pkg/rotation/tier1.go
- [ ] T055 [US3] Implement Tier 2 rotation protocol (coordinated, preflight check) in pkg/rotation/tier2.go
- [ ] T056 [US3] Implement Tier 3 rotation protocol (keypair, add-then-swap) in pkg/rotation/tier3.go
- [ ] T057 [US3] Implement `mesh-ctl rotate` with --tier, --master, --endpoint, --client flags in cmd/mesh-ctl/cmd/rotate.go
- [ ] T058 [US3] Implement `mesh-ctl rotate --scheduled` — check topology intervals, rotate due in cmd/mesh-ctl/cmd/rotate.go
- [ ] T059 [US7] Implement `mesh-ctl capture refresh` — call CaptureRefresh gRPC on masters in cmd/mesh-ctl/cmd/capture.go
- [ ] T060 [P] [US7] Implement `mesh-ctl capture domains --list/--add/--import` in cmd/mesh-ctl/cmd/capture.go
- [ ] T061 Implement CaptureRefresh gRPC handler in awg-mesh-node master mode in pkg/node/master.go
- [ ] T062 Verify: Tier 1 rotation zero packet loss, Tier 2 coordinated, capture from 10+ domains (integration test)

## Phase 5: Client + MikroTik + Address Space (FR-3, FR-11, FR-12)

**Goal:** MikroTik support, Linux client, address space management CLI.
**Test criteria:** MikroTik container connects to mesh, address range operations work.

- [ ] T063 [US5] Implement MikroTik RouterOS command generation (/container, /interface/veth, /ip/route) in pkg/mikrotik/commands.go
- [ ] T064 [P] [US5] Implement .rsc script templates for MikroTik import in pkg/mikrotik/templates.go
- [ ] T065 [US5] Implement `mesh-ctl client prepare --type mikrotik` with RouterOS output in cmd/mesh-ctl/cmd/client.go
- [ ] T066 [US5] Implement `mesh-ctl rotate --client` with sequential ECMP rotation for MikroTik in cmd/mesh-ctl/cmd/rotate.go
- [ ] T067 [US6] Implement `mesh-ctl ip list` — show all ranges and allocations in cmd/mesh-ctl/cmd/ip.go
- [ ] T068 [P] [US6] Implement `mesh-ctl ip range add/resize/move/rename/delete` in cmd/mesh-ctl/cmd/ip.go
- [ ] T069 [P] [US6] Implement `mesh-ctl ip range set-balancer` in cmd/mesh-ctl/cmd/ip.go
- [ ] T070 [US1] Implement `mesh-ctl endpoint remove` in cmd/mesh-ctl/cmd/endpoint.go
- [ ] T071 [P] [US2] Implement `mesh-ctl master remove` in cmd/mesh-ctl/cmd/master.go
- [ ] T072 [P] Implement `mesh-ctl client remove` in cmd/mesh-ctl/cmd/client.go
- [ ] T073 Verify: MikroTik container connects to master, sequential rotation, ip range operations (integration test)

## Phase 6: Observability + Polish (NFR-4, NFR-6)

**Goal:** Production readiness — metrics, logging, docs, migration.
**Test criteria:** Prometheus scrapes metrics, logs are structured JSON.

- [ ] T074 Implement Prometheus metrics endpoint (:9091/metrics) in pkg/node/metrics.go
- [ ] T075 [P] Implement structured JSON logging (zerolog) across all packages in pkg/logging/
- [ ] T076 [P] Implement `mesh-ctl capture schedule` — set up cron on master in cmd/mesh-ctl/cmd/capture.go
- [ ] T077 Implement graceful shutdown for awg-mesh-node (signal handling, cleanup) in cmd/awg-mesh-node/main.go
- [ ] T078 [P] Write README.md with architecture overview, quickstart, topology reference
- [ ] T079 [P] Write migration guide: current MikroTik 5-container → awg-mesh 2-master
- [ ] T080 Final integration test: full mesh (2 masters, 3 endpoints, 1 MikroTik client), rotation, failover, capture

## Dependencies

```
Phase 0 blocks everything
Phase 1: T007 → T008 (device before UAPI)
          T011 → T012, T013 (parser before ranges/validation)
          T014 → T015, T016 (skeleton before modes)
Phase 2: T018,T019 → T020 → T024,T025,T026 (proto → codegen → server/client)
          T021 → T024 (CA before gRPC server)
          T027 → T028-T035 (cobra skeleton before subcommands)
          T024 → T031-T033 (gRPC server before init commands)
Phase 3: Phase 2 complete → T037 (gRPC required for master management)
          T037 → T038-T041 (master before routing)
          T042 independent of routing (can parallel with T038-T041)
Phase 4: T048-T052 independent of Phase 3 (awggen is a library)
          T053 needs gopacket (may need special Docker setup)
          T054-T056 depend on T048-T052 (generator before rotation)
Phase 5: Phase 3 complete → T063-T066 (master mode required for MikroTik)
          T067-T069 depend only on T011-T013 (topology module)
Phase 6: All functional phases complete → T074-T080
```

## Execution Strategy

- **MVP scope:** Phase 0-3 (T001-T047) — working mesh with master ↔ endpoint, failover
- **Parallel opportunities:** awggen library (T048-T052) can start alongside Phase 3
- **Commit strategy:** one commit per task, PR per phase
- **Total tasks:** 80
- **Phase breakdown:** P0=6, P1=11, P2=19, P3=11, P4=15, P5=11, P6=7
