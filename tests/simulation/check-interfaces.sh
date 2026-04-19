#!/usr/bin/env bash
# Quick diagnostic: start an endpoint container, wait for it to initialize, check interfaces.
set -e

CTR="test-ep-diag-$$"

# Prepare node config using mesh-ctl prepare so we get a real bcrypt token.
TMPDIR=$(mktemp -d)
TOPO="${TMPDIR}/topo.yml"
cat > "${TOPO}" <<'EOF'
overlay:
  subnet: 172.21.92.0/27
  prefix_length: 27
transport:
  subnet: 10.92.0.0/16
  prefix_length: 30
masters:
  - name: mst-ru-01
    wan_ip: 192.168.92.10
    listen_port: 51820
endpoints:
  - name: test-ep
    masters: [mst-ru-01]
    overlay_ip: 172.21.92.2
EOF

MESHCTL_BIN="${1:-mesh-ctl}"
if ! command -v mesh-ctl >/dev/null 2>&1; then
    MESHCTL_BIN="/mnt/d/Dev/awg-mesh-wt/v1122-hotfix/bin/mesh-ctl"
fi

"${MESHCTL_BIN}" --config-dir "${TMPDIR}" prepare endpoint test-ep --topology "${TOPO}" > /dev/null 2>&1
TOKEN=$(cat "${TMPDIR}/nodes/test-ep/mesh.token")
HASH=$(cat "${TMPDIR}/nodes/test-ep/mesh.token.hash" 2>/dev/null || \
    "${MESHCTL_BIN}" --config-dir "${TMPDIR}" prepare endpoint test-ep --topology "${TOPO}" 2>&1 | grep -o '\$2[ab]\$[0-9]*\$[^ ]*' | head -1 || echo "")

# Alternative: write plaintext token file inside container directly
docker run -d --privileged --name "$CTR" awg-mesh-node:local \
    /bin/sh -c "mkdir -p /config && printf '%s' '${TOKEN}' > /config/mesh.token && exec /usr/local/bin/awg-mesh-node --mode endpoint --name test-ep --overlay-ip 172.21.92.2 --listen-port 51820"

sleep 4
echo "=== container status ==="
docker inspect "$CTR" --format '{{.State.Status}} {{.State.ExitCode}}' 2>&1 || true
echo "=== container logs ==="
docker logs "$CTR" 2>&1 | head -30 || true
echo "=== ip link show ==="
docker exec "$CTR" ip link show 2>&1 || echo "(container not running)"
echo "=== done ==="
docker stop "$CTR" > /dev/null 2>&1 || true
docker rm "$CTR" > /dev/null 2>&1 || true
rm -rf "${TMPDIR}"
