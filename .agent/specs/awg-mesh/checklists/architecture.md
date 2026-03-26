# Architecture & Requirements Quality Checklist

**Feature:** AWG Mesh
**Focus:** Full architecture, security, networking, processes
**Created:** 2026-03-26
**Depth:** Deep

## Pre-Implementation Gates

### Repo & Project Setup
- [ ] CHK001 Has the awg-mesh repo been created at D:\Dev\awg-mesh as a separate project (not inside nvmd-devops)? [Setup, Gate]
- [ ] CHK002 Has the repo been initialized with Go module, CLAUDE.md, AGENTS.md, .agent/ per onboarding standard? [Setup, Gate]
- [ ] CHK003 Has a project constitution been written at .agent/specs/constitution.md defining non-negotiable principles? [Constitution, Gate]
- [ ] CHK004 Has the constitution been reviewed and approved before implementation starts? [Constitution, Gate]

### Analyze Findings Resolved
- [ ] CHK005 Has FR-10 "except thin mode" stale reference been removed? [Consistency, Spec §FR-10, Analyze A1]
- [ ] CHK006 Has NFR-2 token description been updated to permanent MESH_TOKEN with dual auth? [Consistency, Spec §NFR-2, Analyze A2]
- [ ] CHK007 Has FR-9 "token invalidated" been corrected to "mTLS becomes primary, token remains fallback"? [Consistency, Spec §FR-9, Analyze A3]
- [ ] CHK008 Has FR-12 been updated to reflect MikroTik full gRPC + UAPI capability? [Consistency, Spec §FR-12, Analyze A4]
- [ ] CHK009 Has bootstrap task been added to tasks.md? [Coverage, Analyze C1]
- [ ] CHK010 Has startup reconciliation task been added to tasks.md? [Coverage, Spec §NFR-3, Analyze C2]
- [ ] CHK011 Is MESH_TOKEN terminology consistent across all artifacts (not INIT_TOKEN)? [Consistency, Analyze E1]

## Requirement Completeness

### Unified Node Binary (FR-1)
- [ ] CHK012 Are all four modes (master/endpoint/client/thin→removed) defined with distinct responsibilities? [Completeness, Spec §FR-1]
- [ ] CHK013 Is it specified what happens when awg-mesh-node starts with no config (first boot vs corrupted config)? [Completeness, Edge Case]
- [ ] CHK014 Is the startup sequence documented (create interfaces → load peers → start gRPC → healthcheck)? [Completeness, Gap]

### Topology (FR-2)
- [ ] CHK015 Are all topology.yml fields defined with types, defaults, and validation rules? [Completeness, Spec §FR-2]
- [ ] CHK016 Is the schema versioned (what happens when topology format evolves)? [Completeness, Gap]

### Address Space (FR-3)
- [ ] CHK017 Is the behavior defined when auto-allocation exhausts a range? [Completeness, Edge Case]
- [ ] CHK018 Is range resize behavior defined when it would orphan the balancer IP? [Completeness, Edge Case]
- [ ] CHK019 Are CIDR and explicit range notations both documented with examples? [Completeness, Spec §FR-3]

### Load Balancing (FR-4)
- [ ] CHK020 Is the ECMP hash algorithm specified (or explicitly delegated to kernel)? [Clarity, Spec §FR-4]
- [ ] CHK021 Is the behavior defined when a balancer IP has zero healthy backends? [Completeness, Edge Case]
- [ ] CHK022 Are weight semantics for ECMP documented (how weight=50 vs weight=100 affects distribution)? [Clarity]

### Healthcheck (FR-5)
- [ ] CHK023 Are default values for interval, timeout, failure threshold specified? [Completeness, Spec §FR-5]
- [ ] CHK024 Is the health check method defined (ICMP ping to overlay IP? TCP? gRPC health?)? [Clarity, Spec §FR-5]
- [ ] CHK025 Is the behavior defined when ALL endpoints in a region fail health check? [Completeness, Edge Case]

### Rotation (FR-6)
- [ ] CHK026 Is Tier 2 preflight check explicitly defined (what exactly is checked before proceeding)? [Clarity, Spec §FR-6]
- [ ] CHK027 Is rollback behavior defined for each tier (what state to restore on failure)? [Completeness, Spec §FR-6]
- [ ] CHK028 Is the rotation window (time between apply-to-endpoint and apply-to-master) bounded? [Clarity, Gap]
- [ ] CHK029 Is the Tier 2 offline-client deadlock problem documented in spec edge cases? [Coverage, Edge Case]

### Param Generation (FR-7)
- [ ] CHK030 Are the 9 protocol families listed and defined (not just counted)? [Completeness, Spec §FR-7]
- [ ] CHK031 Is the minimum viable capture dataset defined (how many .bin files needed)? [Completeness, Gap]
- [ ] CHK032 Is fallback behavior defined when no capture data exists (use built-in defaults? error?)? [Completeness, Edge Case]

### Onboarding (FR-9)
- [ ] CHK033 Is the init idempotency requirement explicit (re-running init on initialized node = safe)? [Completeness, Edge Case]
- [ ] CHK034 Is the token TTL configurable or hardcoded? [Clarity, Spec §FR-9]
- [ ] CHK035 Is the behavior defined when init is called on a node already in mTLS mode? [Completeness, Edge Case]

### gRPC API (FR-10)
- [ ] CHK036 Are all gRPC error codes defined for each RPC method? [Completeness, Gap]
- [ ] CHK037 Is gRPC server port configurable or hardcoded to 9090? [Clarity]

## Requirement Clarity

- [ ] CHK038 Is "zero downtime" for Tier 1 rotation quantified (< X ms packet loss? zero packets?)? [Clarity, Spec §FR-6]
- [ ] CHK039 Is "brief reconnect (~2s)" for Tier 3 quantified with max acceptable downtime? [Clarity, Spec §FR-6]
- [ ] CHK040 Is "configurable schedule" in FR-6 defined with format (cron? duration?)? [Clarity]
- [ ] CHK041 Is "per-master independent capture" in FR-8 clear that capture happens ON the master node (not admin PC)? [Clarity, Spec §FR-8]

## Security Requirements Quality

- [ ] CHK042 Is the CA key protection requirement specified (file permissions, encryption at rest)? [Security, Spec §NFR-2]
- [ ] CHK043 Is the gRPC port exposure defined (bind to 0.0.0.0 or localhost only)? [Security, Gap]
- [ ] CHK044 Is the MESH_TOKEN storage format defined (plaintext? hashed? encrypted?) on the node side? [Security, Gap]
- [ ] CHK045 Are AWG tunnel key rotation events auditable (logged with enough detail for forensics)? [Security, Spec §NFR-6]
- [ ] CHK046 Is the threat model for MESH_TOKEN theft defined (what can attacker do, mitigations)? [Security, Gap]

## Networking Requirements Quality

- [ ] CHK047 Is the AWG overhead value (60 bytes) verified against actual amneziawg-go behavior? [Clarity, Spec §FR-13]
- [ ] CHK048 Are the overlay space and mesh transport space documented as non-overlapping? [Consistency]
- [ ] CHK049 Is NAT placement explicitly defined (endpoint only, or also master)? [Clarity, Gap]
- [ ] CHK050 Is the return path defined (symmetric via same master→endpoint, or asymmetric allowed)? [Clarity, Gap]

## Process Requirements Quality

- [ ] CHK051 Is the `mesh-ctl endpoint remove` cleanup behavior fully defined (what happens to in-flight connections)? [Completeness, Gap]
- [ ] CHK052 Is the order of operations for `mesh-ctl master remove` explicit (MikroTik first or last)? [Completeness]
- [ ] CHK053 Is concurrent mesh-ctl operation defined (two operators running mesh-ctl simultaneously)? [Completeness, Edge Case]

## Constitution Items (to write)

- [ ] CHK054 Does the constitution define the control-plane / data-plane separation principle? [Constitution]
- [ ] CHK055 Does the constitution define the "masters are independent" principle (no inter-master state)? [Constitution]
- [ ] CHK056 Does the constitution define "topology.yml is single source of truth"? [Constitution]
- [ ] CHK057 Does the constitution define "UAPI-first" for config changes (not file replacement + restart)? [Constitution]
- [ ] CHK058 Does the constitution define security posture (mTLS mandatory, no plaintext management)? [Constitution]
- [ ] CHK059 Does the constitution define "no external dependencies" (no etcd, no Consul, no k8s required)? [Constitution]
- [ ] CHK060 Does the constitution define the Go-only rule (single language, single binary per component)? [Constitution]

## Dependency & Assumption Quality

- [ ] CHK061 Is the assumption "amneziawg-go supports N devices per process" verified with test? [Assumption, Verified]
- [ ] CHK062 Is the assumption "Jipok/wgctrl-go supports all AWG params" verified against source? [Assumption, Verified]
- [ ] CHK063 Is the assumption "MikroTik container runtime supports gRPC port exposure" verified? [Assumption, Unverified]
- [ ] CHK064 Is the assumption "gopacket works in Alpine Docker with NET_RAW" verified? [Assumption, Unverified]
- [ ] CHK065 Is the amneziawg-go version pinned or is "latest" acceptable? [Dependency, Gap]
