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

## CONVENTIONS

- Go 1.24+, static build, no CGO
- amneziawg-go as library (import), not subprocess
- All management via gRPC, SSH only for bootstrap
- One binary, multiple modes (--mode master|endpoint|client)
- UAPI-first for runtime config changes
