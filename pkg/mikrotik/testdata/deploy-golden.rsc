# awg-mesh RouterOS deployment script
# Generated for container AWG_MESH_HOME
# Topology node name: mikrotik-home
#
# KEYPAIR NOTE:
# The awg-mesh-client container generates its own AmneziaWG keypair on first boot.
# After running this script and starting the container, run:
#
#   mesh-ctl client init mikrotik-home
#
# from the admin workstation. The init RPC reads the node's public key and
# registers it with all masters via gRPC — no manual key exchange needed.

# === Veth ===
/interface/veth add name=AWG_MESH_HOME address=100.127.0.1/24 gateway=100.127.0.1

# === Bridge (idempotent — shared across awg-mesh containers) ===
/interface/bridge/add name=BR_AWG_MESH comment="awg-mesh container bridge"
/ip/address/add address=100.127.0.1/24 interface=BR_AWG_MESH comment="awg-mesh container gateway"
/interface/bridge/port add bridge=BR_AWG_MESH interface=AWG_MESH_HOME

# === NAT ===
/ip/firewall/nat add chain=srcnat action=masquerade src-address=100.127.0.0/24 comment="awg-mesh: container masquerade"
/ip/firewall/nat add chain=dstnat protocol=tcp dst-port=9090 action=dst-nat to-addresses=100.127.0.1 to-ports=9090 comment="awg-mesh: gRPC management port"

# === Firewall (placed before defconf drop rules) ===
:local fastTrackId [/ip/firewall/filter find where action=fasttrack-connection chain=forward]
:if ([:len $fastTrackId] > 0) do={
    /ip/firewall/filter add chain=forward action=accept connection-state=established,related in-interface=BR_AWG_MESH comment="awg-mesh: established return traffic" place-before=$fastTrackId
    /ip/firewall/filter add chain=forward action=accept in-interface=BR_AWG_MESH comment="awg-mesh: container outbound" place-before=$fastTrackId
} else={
    /ip/firewall/filter add chain=forward action=accept connection-state=established,related in-interface=BR_AWG_MESH comment="awg-mesh: established return traffic"
    /ip/firewall/filter add chain=forward action=accept in-interface=BR_AWG_MESH comment="awg-mesh: container outbound"
    # WARNING: no fasttrack-connection rule found, appended to chain end
    :log warning "awg-mesh: no fasttrack-connection rule, appended to chain end"
}

# === Route: overlay space → container ===
/ip/route add dst-address=10.10.0.0/16 gateway=100.127.0.1 comment="awg-mesh: overlay network"

# === Mount + Container ===
/container/mounts/add list=AWG_MESH_HOME_CONFIG src=/disk1/etc/awg-mesh-client-mikrotik-home-config dst=/config
/container/envs/add list=AWG_MESH_HOME_ENVS key=MESH_MODE value="clientd"
/container/envs/add list=AWG_MESH_HOME_ENVS key=MESH_NAME value="mikrotik-home"
/container/envs/add list=AWG_MESH_HOME_ENVS key=MESH_OVERLAY_IP value="10.10.0.10"
/container/envs/add list=AWG_MESH_HOME_ENVS key=MESH_TOKEN_HASH value=mesh1.AAEBABACAQEABAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyAhIiMkJSYnKCkqKyws
/container/add interface=AWG_MESH_HOME remote-image=ghcr.io/coonfuuseed-paandaa/awg-mesh-client:v1.14.0 hostname=awg-mesh-home root-dir=/disk1/awg-mesh-client-mikrotik-home envlist=AWG_MESH_HOME_ENVS name=AWG_MESH_HOME mountlists=AWG_MESH_HOME_CONFIG dns=1.1.1.1,8.8.8.8 logging=yes start-on-boot=yes

# RouterOS starts the container after local image import; start-on-boot=yes keeps it running after reboot.
