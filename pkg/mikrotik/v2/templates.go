package v2

const staticScriptTemplate = `# awg-mesh v2 RouterOS native WireGuard client script
# Mesh: {{.MeshName}}
# Client: {{.ClientName}}

/interface/wireguard
add name={{ros .InterfaceName}} listen-port={{.ListenPort}} private-key={{quote .PrivateKey}} comment={{quote .InterfaceComment}}

/ip/address
add address={{ros .ClientAddress}} interface={{ros .InterfaceName}} comment={{quote .AddressComment}}
{{range .Peers}}
/interface/wireguard/peers
add interface={{ros $.InterfaceName}} public-key={{quote .PublicKey}} endpoint-address={{ros .EndpointAddress}} endpoint-port={{.EndpointPort}} allowed-address={{rosList .AllowedIPs}} persistent-keepalive={{ros .PersistentKeepalive}} comment={{quote .Comment}}
{{end}}`
