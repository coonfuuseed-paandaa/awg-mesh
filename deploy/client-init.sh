#!/usr/bin/env sh
# deploy/client-init.sh — Container ENTRYPOINT for awg-mesh client mode.
# Runs before the Go binary: probes firewall backend, installs MASQUERADE
# + MSS clamp, writes rt_tables when client-state.yml is present, then
# execs the Go binary. POSIX sh (busybox compat). shellcheck-clean.

set -eu

###############################################################################
# 1. Detect firewall backend
###############################################################################

MESH_FW_BACKEND=""

if nft list ruleset >/dev/null 2>&1; then
    MESH_FW_BACKEND="nftables"
elif iptables-legacy -L >/dev/null 2>&1; then
    MESH_FW_BACKEND="iptables-legacy"
else
    echo "client-init: ERROR: no usable firewall backend (nftables or iptables-legacy required)" >&2
    exit 1
fi

echo "client-init: firewall backend: ${MESH_FW_BACKEND}"

###############################################################################
# 2 + 3. Install MASQUERADE on wg-c* and MSS clamp on FORWARD (idempotent)
###############################################################################

if [ "${MESH_FW_BACKEND}" = "nftables" ]; then
    # Create table + chains if missing (all add operations are idempotent)
    nft add table ip awg_mesh_client 2>/dev/null || true
    nft add chain ip awg_mesh_client postrouting \
        '{ type nat hook postrouting priority 100; policy accept; }' 2>/dev/null || true
    nft add chain ip awg_mesh_client forward \
        '{ type filter hook forward priority mangle; policy accept; }' 2>/dev/null || true

    # MASQUERADE on wg-c* egress — add only when the exact rule is absent
    if ! nft list chain ip awg_mesh_client postrouting 2>/dev/null \
            | grep -q 'oifname.*wg-c.*masquerade'; then
        nft add rule ip awg_mesh_client postrouting \
            'oifname "wg-c*" masquerade'
        echo "client-init: nft: installed MASQUERADE on wg-c*"
    else
        echo "client-init: nft: MASQUERADE on wg-c* already present (skipping)"
    fi

    # MSS clamp on FORWARD
    if ! nft list chain ip awg_mesh_client forward 2>/dev/null \
            | grep -q 'tcp option maxseg size set rt mtu'; then
        nft add rule ip awg_mesh_client forward \
            'tcp flags syn tcp option maxseg size set rt mtu'
        echo "client-init: nft: installed MSS clamp on FORWARD"
    else
        echo "client-init: nft: MSS clamp on FORWARD already present (skipping)"
    fi

else
    # iptables-legacy path

    # MASQUERADE — idempotent via -C check
    if ! iptables-legacy -t nat -C POSTROUTING -o 'wg-c+' -j MASQUERADE 2>/dev/null; then
        iptables-legacy -t nat -A POSTROUTING -o 'wg-c+' -j MASQUERADE
        echo "client-init: iptables-legacy: installed MASQUERADE on wg-c+"
    else
        echo "client-init: iptables-legacy: MASQUERADE on wg-c+ already present (skipping)"
    fi

    # MSS clamp on FORWARD — idempotent via -C check
    if ! iptables-legacy -t mangle -C FORWARD -p tcp --tcp-flags SYN,RST SYN \
            -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null; then
        iptables-legacy -t mangle -A FORWARD -p tcp --tcp-flags SYN,RST SYN \
            -j TCPMSS --clamp-mss-to-pmtu
        echo "client-init: iptables-legacy: installed MSS clamp on FORWARD"
    else
        echo "client-init: iptables-legacy: MSS clamp on FORWARD already present (skipping)"
    fi
fi

###############################################################################
# 4. Write rt_tables from /config/client-state.yml (optional — no yq needed)
###############################################################################
# Expected YAML structure (lines of interest):
#   rt_tables:
#     - id: 100
#       name: awg_dscp_af11
#     - id: 101
#       name: awg_dscp_af21
#
# awk extracts id/name pairs and writes them as "id name" lines.
###############################################################################

CLIENT_STATE="/config/client-state.yml"

if [ -f "${CLIENT_STATE}" ]; then
    RT_TABLES_FILE="/etc/iproute2/rt_tables.d/awg-mesh.conf"
    mkdir -p /etc/iproute2/rt_tables.d

    awk '
        /rt_tables:/ { in_section = 1; next }
        in_section && /^[^ ]/ && !/^-/ && !/^  / { in_section = 0 }
        in_section && /id:/ {
            sub(/.*id:[[:space:]]*/, "")
            sub(/[[:space:]]*$/, "")
            table_id = $0
        }
        in_section && /name:/ {
            sub(/.*name:[[:space:]]*/, "")
            sub(/[[:space:]]*$/, "")
            table_name = $0
            if (table_id != "" && table_name != "") {
                print table_id "\t" table_name
                table_id = ""
                table_name = ""
            }
        }
    ' "${CLIENT_STATE}" > "${RT_TABLES_FILE}"

    echo "client-init: wrote ${RT_TABLES_FILE}"
fi

###############################################################################
# 5. Hand off to the Go binary (replace shell process — no orphan shell)
###############################################################################

exec /usr/local/bin/awg-mesh-node "$@"
