# Technical Debt

### 2026-03-27: TLS/QUIC Packet Capture (T053)
**What:** pkg/awggen/capture.go not implemented. CaptureRefresh gRPC handler returns empty response.
**Why:** Requires gopacket (libpcap) which needs special Docker setup with libpcap-dev. Deferred to dedicated Docker testing phase.
**Impact:** AWG param generation uses synthetic protocol templates only. Real packet data for I-spec not available.
**Context:** pkg/grpc/handlers.go CaptureRefresh, T053 in tasks.md

### 2026-03-27: gRPC Hot-Reload After Init
**What:** After Init RPC writes TLS certs, the gRPC server must be restarted to switch from token-only to mTLS mode.
**Why:** Go's tls.Config doesn't support hot certificate reload without server restart. Full graceful hot-reload adds complexity.
**Impact:** After first `mesh-ctl endpoint init`, container needs restart for mTLS. Token auth continues to work.
**Context:** pkg/grpc/server.go, pkg/node/ runners
