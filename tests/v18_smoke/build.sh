#!/usr/bin/env bash
# build.sh — build local Docker images for the v1.8.0 smoke + e2e fixture.
# Builds from the repo root (two levels up from this script) so it picks up
# the locally checked-out source, including any v1.8.0 feature branch changes.
#
# Usage: bash tests/v18_smoke/build.sh
# Produces: awg-mesh-node:local-v18  awg-mesh-client:local-v18
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

if ! command -v docker > /dev/null 2>&1; then
    echo "ERROR: docker not found in PATH. Install Docker 24+ and retry." >&2
    exit 2
fi

if ! docker info > /dev/null 2>&1; then
    echo "ERROR: docker not running or not accessible. Start Docker and retry." >&2
    exit 2
fi

echo "[build] Building awg-mesh-node:local-v18 from ${REPO_ROOT}"
docker build \
    -f "${REPO_ROOT}/deploy/Dockerfile.node" \
    --build-arg VERSION=local-v18 \
    -t awg-mesh-node:local-v18 \
    "${REPO_ROOT}"

echo "[build] Building awg-mesh-client:local-v18 from ${REPO_ROOT}"
docker build \
    -f "${REPO_ROOT}/deploy/Dockerfile.client" \
    --build-arg VERSION=local-v18 \
    -t awg-mesh-client:local-v18 \
    "${REPO_ROOT}"

echo "[build] Images built successfully:"
docker images awg-mesh-node:local-v18 --format "  awg-mesh-node:local-v18  {{.Size}}"
docker images awg-mesh-client:local-v18 --format "  awg-mesh-client:local-v18  {{.Size}}"
