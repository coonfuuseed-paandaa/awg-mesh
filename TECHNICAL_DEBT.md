# Technical Debt

### 2026-03-28: Networking layer not wired end-to-end (G1 VIOLATION)

**What:** The networking stack has the building blocks but they are not connected:

1. **No transport addressing** — WG tunnels use overlay IPs directly instead of auto-allocated point-to-point transport /30 subnets. Overlay should run on top of transport, not be conflated with it.

2. **Balancer IP is a stub** — `balancer_ip` is parsed from topology, stored in struct, but `SetECMPRoute()` is never called. No ECMP routing is actually configured.

3. **One-sided peer exchange** — `mesh-ctl endpoint init` adds endpoint as peer on master via AddTunnel, but master is NOT added as peer on endpoint. WG is point-to-point — both sides need peer config.

4. **No overlay routing on master** — `pkg/routing/route.go` has `AddRoute`/`SetECMPRoute` functions, but nobody calls them during AddTunnel or Init flows.

5. **No NAT on endpoint** — `pkg/routing/nat.go` has `EnableMasquerade`, but endpoint runner never calls it.

6. **Healthcheck not wired to ECMP** — `health.go` fires onDown/onUp callbacks, but they don't update ECMP routes.

**Why:** Implementation was phased by package (wg → grpc → node → routing), but cross-package wiring was never completed. Each package works in isolation but the E2E flow is broken.

**Impact:** Two containers can create AWG interfaces and establish WG handshake (verified), but actual traffic routing through the overlay does not work. `mesh-ctl status` shows nodes, but traffic cannot flow endpoint → NAT → internet.

**Root cause:** Architecture needs transport/overlay separation — currently overlay IPs are assigned directly to wg interfaces, which breaks the point-to-point WG model.

**Fix scope:** Architecture-level change:
- Transport address allocator (auto /30 per tunnel)
- Overlay IPs on lo/dummy, not wg interfaces
- Route overlay through transport next-hops
- Wire ECMP routes from balancer_ip → healthy tunnels
- Wire NAT on endpoint startup
- Bidirectional peer exchange in Init flow
