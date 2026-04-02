# Technical Debt

### 2026-04-02: MikroTik RouterOS < 7.21 incompatible with nftables
**What:** awg-mesh-client container uses `google/nftables` Go library which requires kernel `nf_tables` module. MikroTik RouterOS versions before 7.21 do not load this module.
**Why:** Oracle research verified: nf_tables only loaded in RouterOS 7.21+ (confirmed via forum lsmod output). Pre-7.21 containers can only use iptables.
**Impact:** Client container will fail to create DSCP routing rules on ROS < 7.21. Container starts but DSCP policy routing non-functional.
**Context:** Documented as minimum requirement in README. Alternative: dual-stack implementation (nftables primary, iptables-legacy fallback) — significant effort, deferred. Most MikroTik devices with container support run 7.21+ by now (stable since ~Jan 2026).
