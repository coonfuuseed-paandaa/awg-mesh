# Technical Debt

### 2026-03-27: AWG Interface Wiring (endpoint/client/master runners)
**What:** pkg/node/endpoint.go, client.go, master.go log "AWG interface creation deferred" instead of calling pkg/wg.NewInterface(). The WireGuard/AWG tunnel is not actually created.
**Why:** pkg/wg/interface.go requires linux TUN device + amneziawg-go device library. Development was done on Windows with cross-compilation. Integration requires Docker testing.
**Impact:** No actual encrypted tunnels established. Nodes start but don't route traffic.
**Context:** pkg/wg/interface.go, pkg/node/endpoint.go:38, pkg/node/client.go, pkg/node/master.go

### 2026-03-27: TLS/QUIC Packet Capture (T053)
**What:** pkg/awggen/capture.go not implemented. CaptureRefresh gRPC handler returns empty response.
**Why:** Requires gopacket (libpcap) which needs special Docker setup. Deferred to dedicated Docker testing phase.
**Impact:** AWG param generation cannot use real packet data for I-spec templates. Falls back to synthetic templates.
**Context:** pkg/grpc/handlers.go CaptureRefresh, T053 in tasks.md

### 2026-03-27: RotateParams gRPC Handler Not Wired to UAPI
**What:** RotateParams handler accepts the request and returns success but doesn't actually apply params via UAPI.
**Why:** Requires AWG interface wiring (see first item). UAPI client exists (pkg/wg/uapi.go) but no interface to configure.
**Impact:** Rotation commands succeed at gRPC level but don't change AWG obfuscation parameters.
**Context:** pkg/grpc/handlers.go RotateParams, pkg/wg/uapi.go
