# Technical Debt

### 2026-04-02: DSCP policy routing missing connmark save/restore for return traffic
**What:** `SetupDSCPPolicyRouting()` in `pkg/routing/dscp.go` creates nftables rules for DSCP→fwmark on incoming packets, but does NOT add connmark save/restore rules. Reply packets lose fwmark and fall through to default route instead of the correct policy table.
**Why:** Oracle research verified: Linux fwmark is per-packet, NOT automatically propagated to reply packets. Connmark save/restore is mandatory for bidirectional policy routing. `EnableStickyECMP()` already implements this for ECMP — DSCP routing needs the same.
**Impact:** Return traffic for DSCP-routed connections may use wrong path (default ECMP instead of policy-selected endpoint). TCP connections may work due to conntrack, but UDP and new connections will have asymmetric routing.
**Context:** `pkg/routing/dscp.go:26-94` (SetupDSCPPolicyRouting), `pkg/routing/nftables.go:154-246` (EnableStickyECMP — reference implementation for connmark). Fix: add `ct mark set meta mark` (save) in postrouting and `meta mark set ct mark` (restore) in prerouting to the `awg_dscp` nftables table.

### 2026-04-02: MikroTik RouterOS < 7.21 incompatible with nftables
**What:** awg-mesh-client container uses `google/nftables` Go library which requires kernel `nf_tables` module. MikroTik RouterOS versions before 7.21 do not load this module.
**Why:** Oracle research verified: nf_tables only loaded in RouterOS 7.21+ (confirmed via forum lsmod output). Pre-7.21 containers can only use iptables.
**Impact:** Client container will fail to create DSCP routing rules on ROS < 7.21. Container starts but DSCP policy routing non-functional.
**Context:** Documented as minimum requirement in README. Alternative: dual-stack implementation (nftables primary, iptables-legacy fallback) — significant effort, deferred. Most MikroTik devices with container support run 7.21+ by now (stable since ~Jan 2026).
