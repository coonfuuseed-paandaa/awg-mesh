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
# 4. Write rt_tables from /config/client-state.yml routing_policies section
###############################################################################
# saveClientState (pkg/node/client_state.go) persists the schema below:
#
#   routing_policies:
#     - name: af11
#       dscp: 10
#       targets: [...]
#     - name: af21
#       dscp: 18
#       targets: [...]
#
# The Go writer pkg/routing/dscp.writeRtTables produces lines of the form
# "<id> awg-dscp-<DSCP>" where id = 100 + DSCP. We mirror that exact convention
# here so a fresh container can resolve `ip route show table awg-dscp-N`
# *before* awg-mesh-node has had a chance to invoke the writer itself; if the
# Go binary later runs writeRtTables it overwrites this file with identical
# content, so the two paths converge.
#
# Atomic write via temp file + rename — without it a partial read could see a
# half-written rt_tables.d/awg-mesh.conf and break iproute2 lookups.
#
# No-op when:
#   - client-state.yml is missing (cold container, init not yet run)
#   - the routing_policies section is absent or empty
###############################################################################

CLIENT_STATE="/config/client-state.yml"

if [ -f "${CLIENT_STATE}" ]; then
    RT_TABLES_FILE="/etc/iproute2/rt_tables.d/awg-mesh.conf"
    RT_TABLES_TMP="${RT_TABLES_FILE}.tmp.$$"
    mkdir -p /etc/iproute2/rt_tables.d

    # Extract `dscp:` integer values inside the `routing_policies:` list.
    # Convention: id = 100 + dscp ; name = "awg-dscp-<dscp>" — matches the Go
    # writer in pkg/routing/dscp.go (writeRtTables).
    awk '
        BEGIN { in_section = 0 }
        # Section header
        /^routing_policies:[[:space:]]*$/ { in_section = 1; next }
        # End of section: any non-indented, non-blank, non-list-item line
        in_section && /^[^[:space:]-]/ { in_section = 0 }
        in_section && /dscp:[[:space:]]*[0-9]+/ {
            line = $0
            sub(/.*dscp:[[:space:]]*/, "", line)
            sub(/[^0-9].*$/, "", line)
            if (line ~ /^[0-9]+$/) {
                dscp = line + 0
                if (dscp >= 1 && dscp <= 63) {
                    printf "%d awg-dscp-%d\n", 100 + dscp, dscp
                }
            }
        }
    ' "${CLIENT_STATE}" > "${RT_TABLES_TMP}"

    # Only publish when at least one rt_table line was generated. An empty
    # rt_tables.d/awg-mesh.conf is benign for iproute2 but signals to
    # operators that something went wrong; refusing to overwrite an existing
    # populated file with empty content avoids that confusion when the YAML
    # is missing routing_policies (e.g. early bootstrap state).
    if [ -s "${RT_TABLES_TMP}" ]; then
        mv "${RT_TABLES_TMP}" "${RT_TABLES_FILE}"
        echo "client-init: wrote ${RT_TABLES_FILE} ($(wc -l < "${RT_TABLES_FILE}") tables)"
    else
        rm -f "${RT_TABLES_TMP}"
        echo "client-init: client-state.yml has no routing_policies — skipping ${RT_TABLES_FILE}"
    fi
fi

###############################################################################
# 5. Hand off to the Go binary (replace shell process — no orphan shell)
###############################################################################

exec /usr/local/bin/awg-mesh-node "$@"
