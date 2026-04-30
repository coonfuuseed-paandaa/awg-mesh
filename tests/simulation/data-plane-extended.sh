#!/usr/bin/env bash
# data-plane-extended.sh — F-004 master orchestrator for FR-1..FR-6.
#
# Wires the six F-004 assertion modules into a single runnable harness.
# Spawns an issue-92-derived 5-node topology (2 masters + 3 endpoints) under
# COMPOSE_PROJECT="dpext" so it can co-exist with `issue-92-rotation.sh` on
# the same Docker host, then sequentially invokes the FR modules per
# ADR-001 + ADR-004 (sequential, synchronous — shared topology forbids
# parallel FR-3 conntrack rebuild against FR-1 flow distribution).
#
# Topology bootstrap is INLINED (T-001 lib extract DEFERRED per
# TD-2026-04-30-F-004-T-001-TOPOLOGY-LIB-EXTRACT — Rule of Three not yet
# triggered; second consumer of this same fixture). The bootstrap
# replicates `issue-92-rotation.sh` verbatim with the only differences
# being COMPOSE_PROJECT, MASTER bridge subnet, and host port mapping (so
# both harnesses can run on the same host without conflict).
#
# Multi-step WSL2 / Docker-Desktop guard chain (spec C2):
#   1. uname -s == Linux  (kernel-level Linux check)
#   2. uname -r grep microsoft  (informational — WSL2 detected)
#   3. docker info ServerVersion does NOT contain "desktop"
#      (rejects Docker-Desktop-via-WSL2 wrapper, accepts native WSL2 docker)
#   4. cgroup v2 present  (either /sys/fs/cgroup/unified exists OR
#      /sys/fs/cgroup/cgroup.controllers readable — modern unified hierarchy)
#
# Any guard failure → exit 0 + 3-field informative message:
#   * failed gate name
#   * observed value
#   * suggested remediation
#
# Per-module env-var passing:
#   * COMPOSE_PROJECT=dpext means container names become dpext-mst-ru-01 etc.
#   * Each FR module is invoked with the right MASTER_*_CTR / SRC_*_CTR /
#     etc. env vars set to dpext-prefixed names so its defaults
#     (issue92rot-prefixed) get overridden.
#
# CR-003 expected-fail handling:
#   * FR-1 (200/0 distribution) is fixture-N/A — endpoint→endpoint flows
#     are pre-pinned per (src_ep,dst_ep) pair (no per-flow ECMP without
#     a client-mode container). Module ships as future-regression guard.
#   * FR-6 A1 is fixture-N/A — healthcheck-driven nexthop removal is
#     a client-mode feature; A2+A3 remain valid PASS gates.
#   * Orchestrator EXIT 0 if FR-1 + FR-6 are the ONLY failures (expected
#     state); EXIT 1 on any FR-2/FR-3/FR-4/FR-5 failure (real regression);
#     EXIT 3 if FR-1 OR FR-6 unexpectedly PASS (fixture upgraded — operator
#     should reconcile spec).
#
# Runtime budget: ≤5 min on warm Docker daemon (NFR-4).
#   * Topology bootstrap ~90s
#   * Per FR module ~30-60s
#   * Total realistic: ~5 min
#
# Usage:
#   bash tests/simulation/data-plane-extended.sh [--help]
#
# Exit codes:
#   0  All PASS, OR only FR-1+FR-6 failed (expected fixture-N/A per CR-003)
#   1  Real regression — any of FR-2/FR-3/FR-4/FR-5 failed
#   2  Bootstrap or guard-chain environment failure
#   3  FR-1 or FR-6 unexpectedly PASSed (fixture upgraded — review spec)
#
set -euo pipefail

# ---------------------------------------------------------------------------
# CLI parse.
# ---------------------------------------------------------------------------
for arg in "$@"; do
    case "${arg}" in
        --help|-h)
            sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \?//'
            exit 0
            ;;
        *)
            printf 'Unknown argument: %s (try --help)\n' "${arg}" >&2
            exit 2
            ;;
    esac
done

# ---------------------------------------------------------------------------
# Multi-step WSL2 / Docker-Desktop guard chain (spec C2).
# Any failure → EXIT 0 + 3-field informative message.
# ---------------------------------------------------------------------------
guard_skip() {
    local gate="$1"
    local observed="$2"
    local remediation="$3"
    printf '\n[GUARD] SKIP — host environment does not support F-004 data-plane suite.\n'
    printf '  gate:        %s\n' "${gate}"
    printf '  observed:    %s\n' "${observed}"
    printf '  remediation: %s\n' "${remediation}"
    exit 0
}

# Gate 1: uname -s must be Linux.
UNAME_S="$(uname -s)"
if [[ "${UNAME_S}" != "Linux" ]]; then
    guard_skip "non-linux-host" \
        "uname -s = ${UNAME_S}" \
        "Run inside WSL2 Ubuntu, native Linux host, or a CI Linux runner"
fi

# Gate 2: uname -r — informational WSL2 detection (not a hard fail).
UNAME_R="$(uname -r)"
if printf '%s\n' "${UNAME_R}" | grep -qi microsoft; then
    printf '[GUARD] WSL2 kernel detected (uname -r = %s) — proceeding with native-WSL2 path.\n' "${UNAME_R}"
fi

# Gate 3: docker info ServerVersion must NOT contain "desktop".
if ! command -v docker > /dev/null 2>&1; then
    guard_skip "docker-not-installed" \
        "command -v docker returned non-zero" \
        "Install Docker Engine (https://docs.docker.com/engine/install/)"
fi
DOCKER_SERVER_VERSION="$(docker info --format '{{.ServerVersion}}' 2>/dev/null || true)"
if [[ -z "${DOCKER_SERVER_VERSION}" ]]; then
    guard_skip "docker-daemon-unreachable" \
        "docker info returned empty ServerVersion" \
        "Start docker daemon and verify with 'docker info'"
fi
if printf '%s\n' "${DOCKER_SERVER_VERSION}" | grep -qi desktop; then
    guard_skip "docker-desktop-detected" \
        "docker info ServerVersion = ${DOCKER_SERVER_VERSION}" \
        "Install native Docker Engine on Linux/WSL2 (Docker Desktop netns wrapper unsupported)"
fi

# Gate 4: cgroup v2 must be present.
# Two valid signals: legacy /sys/fs/cgroup/unified directory OR modern
# unified hierarchy (cgroup.controllers readable at /sys/fs/cgroup root).
if [[ ! -d /sys/fs/cgroup/unified && ! -r /sys/fs/cgroup/cgroup.controllers ]]; then
    guard_skip "cgroup-v2-absent" \
        "neither /sys/fs/cgroup/unified nor /sys/fs/cgroup/cgroup.controllers found" \
        "Boot host with cgroup v2 enabled (kernel cmdline systemd.unified_cgroup_hierarchy=1)"
fi

printf '[GUARD] All 4 gates passed (Linux + native-docker + cgroup-v2). Proceeding.\n'

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
MODULES_DIR="${SCRIPT_DIR}/modules"

# ---------------------------------------------------------------------------
# Topology constants (parallels issue-92-rotation.sh with COMPOSE_PROJECT
# changed and bridge subnet shifted to 192.168.93.0/24 to avoid clashes).
# ---------------------------------------------------------------------------
COMPOSE_PROJECT="dpext"

MASTER_RU_01="mst-ru-01"
MASTER_RU_02="mst-ru-02"
ENDPOINT_US_01="ep-us-01"
ENDPOINT_ASIA_01="node-asia-01"
ENDPOINT_ASIA_02="node-asia-02"

CTR_MASTER_RU_01="${COMPOSE_PROJECT}-${MASTER_RU_01}"
CTR_MASTER_RU_02="${COMPOSE_PROJECT}-${MASTER_RU_02}"
CTR_ENDPOINT_US_01="${COMPOSE_PROJECT}-${ENDPOINT_US_01}"
CTR_ENDPOINT_ASIA_01="${COMPOSE_PROJECT}-${ENDPOINT_ASIA_01}"
CTR_ENDPOINT_ASIA_02="${COMPOSE_PROJECT}-${ENDPOINT_ASIA_02}"

GRPC_READY_TIMEOUT=60

# Overlay plan — same as issue-92 (separate compose project namespaces docker
# resources; overlay IPs are inside container netns and do not collide).
MASTER_RU_01_OVERLAY="172.21.92.2"
MASTER_RU_02_OVERLAY="172.21.92.3"
ENDPOINT_US_01_OVERLAY="172.21.92.34"
ENDPOINT_ASIA_01_OVERLAY="172.21.92.35"
ENDPOINT_ASIA_02_OVERLAY="172.21.92.36"
ENDPOINTS_RANGE_CIDR="172.21.92.32/27"

# Bridge subnet shifted to 192.168.93.0/24 (issue-92 uses .92.x) to avoid
# Docker bridge conflict if both harnesses run on the same host concurrently.
MASTER_RU_01_BRIDGE="192.168.93.10"
MASTER_RU_02_BRIDGE="192.168.93.11"
ENDPOINT_US_01_BRIDGE="192.168.93.20"
ENDPOINT_ASIA_01_BRIDGE="192.168.93.21"
ENDPOINT_ASIA_02_BRIDGE="192.168.93.22"

NODE_IMAGE="${IMAGE:-awg-mesh-node:local}"

# Working files — created lazily after guards pass so we never leak temp dirs.
CTL_CONFIG_DIR=""
TOPO_FILE=""
COMPOSE_FILE=""

# ---------------------------------------------------------------------------
# Cleanup trap — always tears the orchestrator-owned topology down.
# ---------------------------------------------------------------------------
dpext::cleanup() {
    local rc=$?
    printf '\n[cleanup] Tearing down dpext containers and temp files...\n'
    if [[ -n "${COMPOSE_FILE}" && -f "${COMPOSE_FILE}" ]]; then
        docker compose -p "${COMPOSE_PROJECT}" -f "${COMPOSE_FILE}" \
            down -v --remove-orphans > /dev/null 2>&1 || true
    fi
    [[ -n "${TOPO_FILE}" && -f "${TOPO_FILE}" ]] && rm -f "${TOPO_FILE}" || true
    [[ -n "${COMPOSE_FILE}" && -f "${COMPOSE_FILE}" ]] && rm -f "${COMPOSE_FILE}" || true
    [[ -n "${CTL_CONFIG_DIR}" && -d "${CTL_CONFIG_DIR}" ]] && rm -rf "${CTL_CONFIG_DIR}" || true
    return "${rc}"
}
trap dpext::cleanup EXIT
trap 'printf "[err-trap] line %s: command exited %s — last cmd: %s\n" "${LINENO}" "$?" "${BASH_COMMAND}" >&2' ERR

# ---------------------------------------------------------------------------
# Preflight — docker image + mesh-ctl binary.
# ---------------------------------------------------------------------------
if ! docker image inspect "${NODE_IMAGE}" > /dev/null 2>&1; then
    printf '[preflight] ERROR: image %s not found.\n' "${NODE_IMAGE}" >&2
    printf '            Build it first:  docker build -t %s -f deploy/Dockerfile.node .\n' "${NODE_IMAGE}" >&2
    exit 2
fi

MESHCTL_BIN=""
if command -v mesh-ctl > /dev/null 2>&1; then
    MESHCTL_BIN="mesh-ctl"
elif [[ -x "${REPO_ROOT}/bin/mesh-ctl" ]]; then
    MESHCTL_BIN="${REPO_ROOT}/bin/mesh-ctl"
elif [[ -x "${REPO_ROOT}/bin/mesh-ctl-linux" ]]; then
    MESHCTL_BIN="${REPO_ROOT}/bin/mesh-ctl-linux"
else
    printf '[preflight] ERROR: mesh-ctl not in PATH and not at %s/bin/mesh-ctl[-linux].\n' "${REPO_ROOT}" >&2
    printf '            Install:  go install %s/cmd/mesh-ctl\n' "${REPO_ROOT}" >&2
    exit 2
fi
printf '[preflight] mesh-ctl: %s\n' "$(${MESHCTL_BIN} version 2>&1 | head -1 || echo 'version unknown')"

# jq is consumed by fr2-iperf3-baseline.sh on the host (parses iperf3 -J output
# and reads/writes baseline JSON). Warn-and-continue so FR-2 fails loud rather
# than the orchestrator silently skipping it.
if ! command -v jq > /dev/null 2>&1; then
    printf '[preflight] WARNING: jq not in PATH — FR-2 will fail (host-side dep).\n' >&2
    printf '            Install on Debian/Ubuntu: apt-get install -y jq\n' >&2
fi

# ---------------------------------------------------------------------------
# topo::generate_files — ephemeral topo + compose YAML.
# ---------------------------------------------------------------------------
topo::generate_files() {
    CTL_CONFIG_DIR=$(mktemp -d /tmp/dpext-ctl-XXXXXX)
    TOPO_FILE=$(mktemp /tmp/dpext-topo-XXXXXX.yml)
    COMPOSE_FILE=$(mktemp /tmp/dpext-compose-XXXXXX.yml)

    cat > "${TOPO_FILE}" <<EOF
overlay:
  space: 172.21.92.0/24
  physical_mtu: 1500
  awg_overhead: 80
  ranges:
    - name: masters
      cidr: 172.21.92.0/27
      balancer_ip: 172.21.92.1
    - name: endpoints
      cidr: ${ENDPOINTS_RANGE_CIDR}
      balancer_ip: 172.21.92.33
    - name: clients
      cidr: 172.21.92.128/25

masters:
  - name: ${MASTER_RU_01}
    host: 127.0.0.1
    peer_host: ${MASTER_RU_01_BRIDGE}
    overlay_ip: ${MASTER_RU_01_OVERLAY}
    listen_port: 51820
    grpc_port: 19390
    endpoints:
      - ${ENDPOINT_US_01}
      - ${ENDPOINT_ASIA_01}
      - ${ENDPOINT_ASIA_02}

  - name: ${MASTER_RU_02}
    host: 127.0.0.1
    peer_host: ${MASTER_RU_02_BRIDGE}
    overlay_ip: ${MASTER_RU_02_OVERLAY}
    listen_port: 51820
    grpc_port: 29390
    endpoints:
      - ${ENDPOINT_US_01}
      - ${ENDPOINT_ASIA_01}
      - ${ENDPOINT_ASIA_02}

endpoints:
  - name: ${ENDPOINT_US_01}
    host: 127.0.0.1
    peer_host: ${ENDPOINT_US_01_BRIDGE}
    overlay_ip: ${ENDPOINT_US_01_OVERLAY}
    listen_port: 51820
    grpc_port: 39390

  - name: ${ENDPOINT_ASIA_01}
    host: 127.0.0.1
    peer_host: ${ENDPOINT_ASIA_01_BRIDGE}
    overlay_ip: ${ENDPOINT_ASIA_01_OVERLAY}
    listen_port: 51820
    grpc_port: 49390

  - name: ${ENDPOINT_ASIA_02}
    host: 127.0.0.1
    peer_host: ${ENDPOINT_ASIA_02_BRIDGE}
    overlay_ip: ${ENDPOINT_ASIA_02_OVERLAY}
    listen_port: 51820
    grpc_port: 59390

transport:
  pool: 10.93.0.0/16
  prefix_length: 30
EOF
    printf '[topo] topology file written: %s\n' "${TOPO_FILE}"
}

# ---------------------------------------------------------------------------
# topo::prepare_nodes — pre-prepare every node so containers boot with the
# correct bcrypt token hash. Returns escaped tokens via global variables.
# ---------------------------------------------------------------------------
topo::prepare_nodes() {
    printf '[topo] Pre-preparing nodes to generate auth tokens...\n'
    local node
    for node in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
        ${MESHCTL_BIN} \
            --topology "${TOPO_FILE}" \
            --config-dir "${CTL_CONFIG_DIR}" \
            master prepare "${node}" > /dev/null 2>&1 || {
            printf '[topo] ERROR: mesh-ctl master prepare %s failed\n' "${node}" >&2
            return 3
        }
    done
    for node in "${ENDPOINT_US_01}" "${ENDPOINT_ASIA_01}" "${ENDPOINT_ASIA_02}"; do
        ${MESHCTL_BIN} \
            --topology "${TOPO_FILE}" \
            --config-dir "${CTL_CONFIG_DIR}" \
            endpoint prepare "${node}" > /dev/null 2>&1 || {
            printf '[topo] ERROR: mesh-ctl endpoint prepare %s failed\n' "${node}" >&2
            return 3
        }
    done

    TOKEN_MASTER_RU_01=$(cat "${CTL_CONFIG_DIR}/nodes/${MASTER_RU_01}/mesh.token")
    TOKEN_MASTER_RU_02=$(cat "${CTL_CONFIG_DIR}/nodes/${MASTER_RU_02}/mesh.token")
    TOKEN_ENDPOINT_US_01=$(cat "${CTL_CONFIG_DIR}/nodes/${ENDPOINT_US_01}/mesh.token")
    TOKEN_ENDPOINT_ASIA_01=$(cat "${CTL_CONFIG_DIR}/nodes/${ENDPOINT_ASIA_01}/mesh.token")
    TOKEN_ENDPOINT_ASIA_02=$(cat "${CTL_CONFIG_DIR}/nodes/${ENDPOINT_ASIA_02}/mesh.token")

    # Escape $ -> $$ so docker-compose does not expand bcrypt $2a$12$... refs.
    TOKEN_MASTER_RU_01_ESC="${TOKEN_MASTER_RU_01//\$/\$\$}"
    TOKEN_MASTER_RU_02_ESC="${TOKEN_MASTER_RU_02//\$/\$\$}"
    TOKEN_ENDPOINT_US_01_ESC="${TOKEN_ENDPOINT_US_01//\$/\$\$}"
    TOKEN_ENDPOINT_ASIA_01_ESC="${TOKEN_ENDPOINT_ASIA_01//\$/\$\$}"
    TOKEN_ENDPOINT_ASIA_02_ESC="${TOKEN_ENDPOINT_ASIA_02//\$/\$\$}"
}

# ---------------------------------------------------------------------------
# topo::generate_compose — write the docker-compose YAML with pre-seeded
# MESH_TOKEN_HASH per node, mirroring issue-92-rotation.sh structure.
# ---------------------------------------------------------------------------
topo::generate_compose() {
    cat > "${COMPOSE_FILE}" <<EOF
# Auto-generated by data-plane-extended.sh — do not edit.
services:
  ${MASTER_RU_01}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_MASTER_RU_01}
    hostname: ${MASTER_RU_01}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_MASTER_RU_01_ESC}"
    networks:
      dpext:
        ipv4_address: ${MASTER_RU_01_BRIDGE}
    ports:
      - "19390:9090"
      - "19391:9091"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode master \\
          --name ${MASTER_RU_01} \\
          --overlay-ip ${MASTER_RU_01_OVERLAY} \\
          --listen-port 51820

  ${MASTER_RU_02}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_MASTER_RU_02}
    hostname: ${MASTER_RU_02}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_MASTER_RU_02_ESC}"
    networks:
      dpext:
        ipv4_address: ${MASTER_RU_02_BRIDGE}
    ports:
      - "29390:9090"
      - "29391:9091"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode master \\
          --name ${MASTER_RU_02} \\
          --overlay-ip ${MASTER_RU_02_OVERLAY} \\
          --listen-port 51820

  ${ENDPOINT_US_01}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_ENDPOINT_US_01}
    hostname: ${ENDPOINT_US_01}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_ENDPOINT_US_01_ESC}"
    networks:
      dpext:
        ipv4_address: ${ENDPOINT_US_01_BRIDGE}
    ports:
      - "39390:9090"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode endpoint \\
          --name ${ENDPOINT_US_01} \\
          --overlay-ip ${ENDPOINT_US_01_OVERLAY} \\
          --listen-port 51820

  ${ENDPOINT_ASIA_01}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_ENDPOINT_ASIA_01}
    hostname: ${ENDPOINT_ASIA_01}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_ENDPOINT_ASIA_01_ESC}"
    networks:
      dpext:
        ipv4_address: ${ENDPOINT_ASIA_01_BRIDGE}
    ports:
      - "49390:9090"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode endpoint \\
          --name ${ENDPOINT_ASIA_01} \\
          --overlay-ip ${ENDPOINT_ASIA_01_OVERLAY} \\
          --listen-port 51820

  ${ENDPOINT_ASIA_02}:
    image: ${NODE_IMAGE}
    container_name: ${CTR_ENDPOINT_ASIA_02}
    hostname: ${ENDPOINT_ASIA_02}
    restart: "no"
    privileged: true
    environment:
      MESH_TOKEN_HASH: "${TOKEN_ENDPOINT_ASIA_02_ESC}"
    networks:
      dpext:
        ipv4_address: ${ENDPOINT_ASIA_02_BRIDGE}
    ports:
      - "59390:9090"
    entrypoint:
      - sh
      - -c
      - |
        [ -f /config/mesh.token ] || printf '%s' "\$\$MESH_TOKEN_HASH" > /config/mesh.token
        exec /usr/local/bin/awg-mesh-node \\
          --mode endpoint \\
          --name ${ENDPOINT_ASIA_02} \\
          --overlay-ip ${ENDPOINT_ASIA_02_OVERLAY} \\
          --listen-port 51820

networks:
  dpext:
    driver: bridge
    ipam:
      config:
        - subnet: 192.168.93.0/24
EOF
    printf '[topo] compose file written: %s\n' "${COMPOSE_FILE}"
}

# ---------------------------------------------------------------------------
# topo::compose_up — bring up containers and wait for both masters' gRPC.
# ---------------------------------------------------------------------------
topo::compose_up() {
    printf '[topo] docker compose up -d (project=%s)...\n' "${COMPOSE_PROJECT}"
    docker compose -p "${COMPOSE_PROJECT}" -f "${COMPOSE_FILE}" up -d > /dev/null
}

topo::wait_for_log() {
    local container="$1"
    local pattern="$2"
    local timeout="${3:-${GRPC_READY_TIMEOUT}}"
    local deadline
    deadline=$(( $(date +%s) + timeout ))
    while true; do
        if docker logs "${container}" 2>&1 | grep -q "${pattern}"; then
            return 0
        fi
        if [[ $(date +%s) -ge ${deadline} ]]; then
            return 1
        fi
        sleep 2
    done
}

topo::wait_grpc() {
    printf '[topo] Waiting for masters to report gRPC ready (up to %ss)...\n' "${GRPC_READY_TIMEOUT}"
    local m
    for m in "${CTR_MASTER_RU_01}" "${CTR_MASTER_RU_02}"; do
        if topo::wait_for_log "${m}" "gRPC server listening" "${GRPC_READY_TIMEOUT}"; then
            printf '[topo]   %s gRPC ready\n' "${m}"
        else
            printf '[topo] ERROR: %s did not become gRPC ready within %ss\n' "${m}" "${GRPC_READY_TIMEOUT}" >&2
            docker logs "${m}" >&2 || true
            return 2
        fi
    done
    sleep 3
}

# ---------------------------------------------------------------------------
# topo::init_nodes — `mesh-ctl master/endpoint init` for all 5 nodes.
# ---------------------------------------------------------------------------
topo::init_nodes() {
    printf '[topo] Initialising masters...\n'
    local n
    for n in "${MASTER_RU_01}" "${MASTER_RU_02}"; do
        ${MESHCTL_BIN} \
            --topology "${TOPO_FILE}" \
            --config-dir "${CTL_CONFIG_DIR}" \
            master init "${n}" > /dev/null 2>&1 || {
            printf '[topo] ERROR: master init %s failed\n' "${n}" >&2
            return 2
        }
    done
    printf '[topo] Initialising endpoints...\n'
    for n in "${ENDPOINT_US_01}" "${ENDPOINT_ASIA_01}" "${ENDPOINT_ASIA_02}"; do
        ${MESHCTL_BIN} \
            --topology "${TOPO_FILE}" \
            --config-dir "${CTL_CONFIG_DIR}" \
            endpoint init "${n}" > /dev/null 2>&1 || {
            printf '[topo] ERROR: endpoint init %s failed\n' "${n}" >&2
            return 2
        }
    done
    sleep 2
}

# ---------------------------------------------------------------------------
# topo::warmup_data_plane — issue-92 R10-pattern handshake refresh: best-effort
# pings between every endpoint pair so the AmneziaWG userspace driver
# completes initial handshakes before FR modules try to assert. Without this
# warm-up FR-4/FR-5/FR-6 fail with "pre-flight ping failed" because the very
# first packet on a cold tunnel races against handshake establishment and
# gets dropped by the timeout.
#
# Then verify reachability with a real ping; fail bootstrap if any endpoint
# pair cannot reach each other.
# ---------------------------------------------------------------------------
topo::warmup_data_plane() {
    printf '[topo] warmup: refreshing endpoint↔master handshakes...\n'
    local pairs=(
        "${CTR_ENDPOINT_US_01}:${ENDPOINT_US_01_OVERLAY}"
        "${CTR_ENDPOINT_ASIA_01}:${ENDPOINT_ASIA_01_OVERLAY}"
        "${CTR_ENDPOINT_ASIA_02}:${ENDPOINT_ASIA_02_OVERLAY}"
    )
    local src_entry src_ctr dst
    for src_entry in "${pairs[@]}"; do
        src_ctr="${src_entry%%:*}"
        for dst_entry in "${pairs[@]}"; do
            dst="${dst_entry##*:}"
            [[ "${dst}" == "${src_entry##*:}" ]] && continue
            # Best-effort warmup; -W 5 covers handshake reset + first packet drop.
            docker exec "${src_ctr}" ping -c 1 -W 5 "${dst}" > /dev/null 2>&1 || true
            docker exec "${src_ctr}" ping -c 1 -W 5 "${dst}" > /dev/null 2>&1 || true
        done
    done

    printf '[topo] verify: data-plane reachability matrix (post-warmup)...\n'
    local fail=0
    for src_entry in "${pairs[@]}"; do
        src_ctr="${src_entry%%:*}"
        for dst_entry in "${pairs[@]}"; do
            dst="${dst_entry##*:}"
            [[ "${dst}" == "${src_entry##*:}" ]] && continue
            if ! docker exec "${src_ctr}" ping -c 2 -W 2 "${dst}" > /dev/null 2>&1; then
                printf '[topo]   FAIL: %s -> %s unreachable\n' "${src_ctr}" "${dst}" >&2
                fail=$(( fail + 1 ))
            fi
        done
    done
    if (( fail > 0 )); then
        printf '[topo] FATAL: data-plane not converged (%s pair(s) unreachable)\n' "${fail}" >&2
        return 2
    fi
    printf '[topo] data-plane converged — all 6 endpoint pairs reachable.\n'
}

# ---------------------------------------------------------------------------
# Bootstrap entry — run all topology phases.
# ---------------------------------------------------------------------------
printf '\n=== F-004 data-plane-extended.sh — orchestrator ===\n\n'
T0=$(date +%s)

printf '[bootstrap] Phase 1/5: generate topo+compose files\n'
topo::generate_files

printf '[bootstrap] Phase 2/5: prepare nodes (auth tokens)\n'
topo::prepare_nodes || { printf '[bootstrap] FATAL: prepare failed\n' >&2; exit 2; }

printf '[bootstrap] Phase 3/5: generate compose, docker compose up\n'
topo::generate_compose
topo::compose_up
topo::wait_grpc || { printf '[bootstrap] FATAL: gRPC wait failed\n' >&2; exit 2; }

printf '[bootstrap] Phase 4/5: init masters + endpoints\n'
topo::init_nodes || { printf '[bootstrap] FATAL: init failed\n' >&2; exit 2; }

printf '[bootstrap] Phase 5/5: warm up data plane + reachability gate\n'
topo::warmup_data_plane || { printf '[bootstrap] FATAL: data-plane warmup failed\n' >&2; exit 2; }

T_BOOTSTRAP=$(( $(date +%s) - T0 ))
printf '[bootstrap] DONE in %ss — topology ready.\n\n' "${T_BOOTSTRAP}"

# Export topology + config-dir for FR modules that resolve their mesh-ctl
# arguments from these env vars (FR-3 conntrack-sticky in particular).
export MESH_CTL_TOPOLOGY="${TOPO_FILE}"
export MESH_CTL_CONFIG_DIR="${CTL_CONFIG_DIR}"

# ---------------------------------------------------------------------------
# FR module dispatch — sequential per ADR-001 + ADR-004.
# Per-module env vars override each module's issue92rot-prefixed defaults.
# ---------------------------------------------------------------------------

# Common env shared across modules — exported so each FR script sees them.
export MASTER_01_CTR="${CTR_MASTER_RU_01}"
export MASTER_02_CTR="${CTR_MASTER_RU_02}"

# FR-1 specific
FR1_ENV=(
    "MASTER_01_CTR=${CTR_MASTER_RU_01}"
    "MASTER_02_CTR=${CTR_MASTER_RU_02}"
    "SRC_ENDPOINT_CTR=${CTR_ENDPOINT_ASIA_01}"
    "SRC_INGRESS_IFACE=wg-${ENDPOINT_ASIA_01}"
    "DST_OVERLAY_IP=${ENDPOINT_ASIA_02_OVERLAY}"
)
# FR-2 specific
FR2_ENV=(
    "IPERF3_CLIENT_CTR=${CTR_ENDPOINT_ASIA_01}"
    "IPERF3_SERVER_CTR=${CTR_ENDPOINT_ASIA_02}"
    "IPERF3_SERVER_OVERLAY=${ENDPOINT_ASIA_02_OVERLAY}"
)
# FR-3 specific
FR3_ENV=(
    "MASTER_01_CTR=${CTR_MASTER_RU_01}"
    "MASTER_02_CTR=${CTR_MASTER_RU_02}"
    "SRC_EP_CTR=${CTR_ENDPOINT_ASIA_01}"
    "SRC_EP_OVERLAY=${ENDPOINT_ASIA_01_OVERLAY}"
    "SRC_EP_NAME=${ENDPOINT_ASIA_01}"
    "DST_EP_CTR=${CTR_ENDPOINT_ASIA_02}"
    "DST_EP_OVERLAY=${ENDPOINT_ASIA_02_OVERLAY}"
)
# FR-4 specific
FR4_ENV=(
    "MASTER_KILL_CTR=${CTR_MASTER_RU_01}"
    "PING_CLIENT_CTR=${CTR_ENDPOINT_ASIA_01}"
    "PING_TARGET_OVERLAY=${ENDPOINT_ASIA_02_OVERLAY}"
)
# FR-5 specific
FR5_ENV=(
    "MASTER_01_CTR=${CTR_MASTER_RU_01}"
    "MASTER_02_CTR=${CTR_MASTER_RU_02}"
    "SRC_EP_CTR=${CTR_ENDPOINT_ASIA_01}"
    "SRC_EP_OVERLAY=${ENDPOINT_ASIA_01_OVERLAY}"
    "SRC_EP_NAME=${ENDPOINT_ASIA_01}"
    "DST_EP_CTR=${CTR_ENDPOINT_ASIA_02}"
    "DST_EP_OVERLAY=${ENDPOINT_ASIA_02_OVERLAY}"
    "DST_EP_NAME=${ENDPOINT_ASIA_02}"
)
# FR-6 specific
FR6_ENV=(
    "MASTER_PAUSE_CTR=${CTR_MASTER_RU_01}"
    "MASTER_OTHER_CTR=${CTR_MASTER_RU_02}"
    "SRC_INITIATOR_CTR=${CTR_ENDPOINT_US_01}"
    "SRC_INITIATOR_OVERLAY=${ENDPOINT_US_01_OVERLAY}"
    "SRC_INITIATOR_NAME=${ENDPOINT_US_01}"
    "DST_TARGET_CTR=${CTR_ENDPOINT_ASIA_02}"
    "TARGET_OVERLAY=${ENDPOINT_ASIA_02_OVERLAY}"
)

# Verdict tracking — name => (verdict, exit_code, elapsed_s).
declare -a FR_NAMES=("FR-1" "FR-2" "FR-3" "FR-4" "FR-5" "FR-6")
declare -A FR_VERDICT
declare -A FR_EXIT
declare -A FR_ELAPSED

dpext::run_module() {
    local fr_id="$1"
    local script_name="$2"
    local description="$3"
    shift 3
    local env_pairs=("$@")
    local script_path="${MODULES_DIR}/${script_name}"

    printf '\n==== %s: %s ====\n' "${fr_id}" "${description}"
    if [[ ! -f "${script_path}" || ! -r "${script_path}" ]]; then
        printf '[%s] ERROR: module script not found at %s\n' "${fr_id}" "${script_path}" >&2
        FR_VERDICT[${fr_id}]="ERROR"
        FR_EXIT[${fr_id}]=2
        FR_ELAPSED[${fr_id}]=0
        return
    fi
    local m_t0
    m_t0=$(date +%s)
    local rc=0
    env "${env_pairs[@]}" bash "${script_path}" || rc=$?
    local m_t1
    m_t1=$(date +%s)
    FR_EXIT[${fr_id}]=${rc}
    FR_ELAPSED[${fr_id}]=$(( m_t1 - m_t0 ))
    if [[ ${rc} -eq 0 ]]; then
        FR_VERDICT[${fr_id}]="PASS"
        printf '[%s] verdict: PASS (rc=0, %ss)\n' "${fr_id}" "${FR_ELAPSED[${fr_id}]}"
    else
        FR_VERDICT[${fr_id}]="FAIL"
        printf '[%s] verdict: FAIL (rc=%s, %ss)\n' "${fr_id}" "${rc}" "${FR_ELAPSED[${fr_id}]}"
    fi
}

dpext::run_module "FR-1" "fr1-flow-distribution.sh" "Flow distribution stats" \
    "${FR1_ENV[@]}"
dpext::run_module "FR-2" "fr2-iperf3-baseline.sh" "iperf3 multi-flow throughput baseline" \
    "${FR2_ENV[@]}"
dpext::run_module "FR-3" "fr3-conntrack-sticky.sh" "Conntrack sticky-session preservation" \
    "${FR3_ENV[@]}"
dpext::run_module "FR-4" "fr4-failover-timing.sh" "Failover timing after master kill" \
    "${FR4_ENV[@]}"
dpext::run_module "FR-5" "fr5-asymmetric.sh" "Asymmetric routing detection" \
    "${FR5_ENV[@]}"
dpext::run_module "FR-6" "fr6-sticky-migration.sh" "Sticky session migration on healthcheck flip" \
    "${FR6_ENV[@]}"

# ---------------------------------------------------------------------------
# Summary + exit code calculation per CR-003 expected-fail handling.
# ---------------------------------------------------------------------------
T_TOTAL=$(( $(date +%s) - T0 ))
printf '\n==== SUMMARY ====\n'
printf '  Bootstrap: %ss\n' "${T_BOOTSTRAP}"
printf '  Total wall-clock: %ss\n' "${T_TOTAL}"
printf '\n'
printf '  %-6s  %-6s  %-7s  %s\n' "Module" "Verdict" "Elapsed" "Notes"
printf '  ------  ------  -------  ------------------------------------------\n'

# CR-003 expected-fail set: FR-1 + FR-6 are fixture-N/A on the issue-92-derived
# topology (no client-mode container). PASS unexpected → flag for review.
EXPECTED_FAIL=("FR-1" "FR-6")
is_expected_fail() {
    local fr="$1"
    local x
    for x in "${EXPECTED_FAIL[@]}"; do
        [[ "${fr}" == "${x}" ]] && return 0
    done
    return 1
}

REAL_REGRESSION_COUNT=0
UNEXPECTED_PASS_COUNT=0
for fr in "${FR_NAMES[@]}"; do
    verdict="${FR_VERDICT[${fr}]:-MISSING}"
    rc="${FR_EXIT[${fr}]:-?}"
    elapsed="${FR_ELAPSED[${fr}]:-?}s"
    note=""
    if is_expected_fail "${fr}"; then
        if [[ "${rc}" == "2" ]]; then
            # rc=2 = environment / setup error in module — NOT the expected
            # fixture-N/A assertion failure. Count as real regression so
            # operators don't silently miss broken module runs (CR-003 only
            # excuses assertion failures, not env errors).
            note="ERROR — module env failure (rc=2; not expected fixture-N/A)"
            REAL_REGRESSION_COUNT=$(( REAL_REGRESSION_COUNT + 1 ))
        elif [[ "${verdict}" == "FAIL" ]]; then
            note="FAIL (fixture-N/A per CR-003 — expected assertion)"
        elif [[ "${verdict}" == "PASS" ]]; then
            note="PASS UNEXPECTED — fixture upgraded? review CR-003"
            UNEXPECTED_PASS_COUNT=$(( UNEXPECTED_PASS_COUNT + 1 ))
        else
            note="${verdict} (rc=${rc})"
        fi
    else
        if [[ "${verdict}" == "FAIL" ]]; then
            note="FAIL — real regression (rc=${rc})"
            REAL_REGRESSION_COUNT=$(( REAL_REGRESSION_COUNT + 1 ))
        elif [[ "${verdict}" == "PASS" ]]; then
            note="PASS"
        else
            note="${verdict} (rc=${rc})"
            REAL_REGRESSION_COUNT=$(( REAL_REGRESSION_COUNT + 1 ))
        fi
    fi
    printf '  %-6s  %-6s  %-7s  %s\n' "${fr}" "${verdict}" "${elapsed}" "${note}"
done

printf '\n'
printf '  Real regressions (FR-2/3/4/5 failed):  %s\n' "${REAL_REGRESSION_COUNT}"
printf '  Unexpected passes (FR-1/6 PASSed):     %s\n' "${UNEXPECTED_PASS_COUNT}"
printf '\n'

# Exit code semantics per task brief:
#   0  = nothing failed OR only FR-1+FR-6 failed (expected fixture-N/A)
#   1  = any of FR-2/FR-3/FR-4/FR-5 failed (real regression)
#   2  = bootstrap or guard-chain environment failure (handled earlier with exit 2)
#   3  = at least one of FR-1+FR-6 unexpectedly PASSed
ORCHESTRATOR_EXIT=0
if (( UNEXPECTED_PASS_COUNT > 0 )); then
    ORCHESTRATOR_EXIT=3
    printf '[result] EXIT 3 — fixture-N/A FR-1/FR-6 unexpectedly PASSed (review CR-003).\n'
elif (( REAL_REGRESSION_COUNT > 0 )); then
    ORCHESTRATOR_EXIT=1
    printf '[result] EXIT 1 — real regression in FR-2/3/4/5.\n'
else
    ORCHESTRATOR_EXIT=0
    printf '[result] EXIT 0 — all PASS, or only FR-1+FR-6 failed (expected per CR-003).\n'
fi

# NFR-4: warn (not fail) if total runtime exceeds 5 min.
if (( T_TOTAL > 300 )); then
    printf '[result] WARNING: total runtime %ss exceeds NFR-4 budget (300s).\n' "${T_TOTAL}"
fi

# Disarm the ERR trap before deliberate non-zero exit so we don't print a
# misleading "command exited 1 — last cmd: exit ..." message.
trap - ERR
exit "${ORCHESTRATOR_EXIT}"
