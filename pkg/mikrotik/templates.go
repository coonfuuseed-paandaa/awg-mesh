package mikrotik

import (
	"fmt"
	"net/netip"
	"strings"
	"text/template"
)

const deployScriptTemplate = `# awg-mesh RouterOS deployment script
# Generated for container {{.ContainerName}}

{{- range .VethCommands }}
{{ . }}
{{- end }}
{{- range .RouteCommands }}
{{ . }}
{{- end }}
{{- range .FirewallCommands }}
{{ . }}
{{- end }}
{{- range .ContainerCommands }}
{{ . }}
{{- end }}
{{ .StartCommand }}
`

const rotateScriptTemplate = `# awg-mesh AWG parameter rotation script
# Generated for container {{.ContainerName}}

:local envList {{ .EnvListLiteral }}
:local params {{ .ParamsLiteral }}
:local existing [/container/envs/find where name=$envList and key="MESH_AWG_PARAMS"]
:if ([:len $existing] = 0) do={
    /container/envs/add name=$envList key=MESH_AWG_PARAMS value=$params
} else={
    /container/envs/set $existing value=$params
}
/container/restart [find where name={{ .ContainerNameLiteral }}]
`

// DeployScript holds data for generating a full RouterOS .rsc deployment script.
type DeployScript struct {
	ContainerName string
	Image         string
	Veth          string
	VethGateway   string
	OverlayIP     string
	OverlayNet    string
	ListenPort    int
	Masters       []string
	AWGConfig     string
	Token         string
}

// GenerateDeployRSC generates a full .rsc script importable via /import.
func GenerateDeployRSC(ds DeployScript) (string, error) {
	if err := validateDeployScript(ds); err != nil {
		return "", err
	}

	_, normalizedGateway := deriveVethAddressAndGateway(ds.VethGateway)
	envVars := buildDeployEnvVars(ds)
	containerCfg := ContainerConfig{
		Name:        ds.ContainerName,
		Image:       ds.Image,
		Interface:   ds.Veth,
		VethGateway: normalizedGateway,
		OverlayIP:   ds.OverlayIP,
		ListenPort:  ds.ListenPort,
		EnvVars:     envVars,
	}

	templateData := struct {
		ContainerName     string
		VethCommands      []string
		RouteCommands     []string
		FirewallCommands  []string
		ContainerCommands []string
		StartCommand      string
	}{
		ContainerName:     ds.ContainerName,
		VethCommands:      GenerateVethCommands(ds.Veth, ds.VethGateway),
		RouteCommands:     GenerateRouteCommands(ds.OverlayNet, normalizedGateway),
		FirewallCommands:  GenerateFirewallCommands(ds.Veth),
		ContainerCommands: GenerateContainerCommands(containerCfg),
		StartCommand:      fmt.Sprintf("/container/start [find where name=%s]", escapeRouterOSToken(ds.ContainerName)),
	}

	script, err := executeTemplate("deploy-rsc", deployScriptTemplate, templateData)
	if err != nil {
		return "", fmt.Errorf("render deploy script: %w", err)
	}
	return script, nil
}

// RotateParams holds parameters for an AWG rotation script.
type RotateParams struct {
	Jc   int
	Jmin int
	Jmax int
	S1   int
	S2   int
	H1   int
	H2   int
	H3   int
	H4   int
}

// GenerateRotateRSC generates a minimal RouterOS script to update AWG parameters.
func GenerateRotateRSC(containerName string, params RotateParams) (string, error) {
	trimmedName := strings.TrimSpace(containerName)
	if trimmedName == "" {
		return "", fmt.Errorf("container name is required")
	}
	if err := validateRotateParams(params); err != nil {
		return "", err
	}

	encodedParams := fmt.Sprintf(
		"jc=%d,jmin=%d,jmax=%d,s1=%d,s2=%d,h1=%d,h2=%d,h3=%d,h4=%d",
		params.Jc,
		params.Jmin,
		params.Jmax,
		params.S1,
		params.S2,
		params.H1,
		params.H2,
		params.H3,
		params.H4,
	)

	templateData := struct {
		ContainerName        string
		EnvListLiteral       string
		ParamsLiteral        string
		ContainerNameLiteral string
	}{
		ContainerName:        trimmedName,
		EnvListLiteral:       quoteRouterOSValue(buildEnvListName(trimmedName)),
		ParamsLiteral:        quoteRouterOSValue(encodedParams),
		ContainerNameLiteral: quoteRouterOSValue(trimmedName),
	}

	script, err := executeTemplate("rotate-rsc", rotateScriptTemplate, templateData)
	if err != nil {
		return "", fmt.Errorf("render rotate script: %w", err)
	}
	return script, nil
}

func validateDeployScript(ds DeployScript) error {
	trimmedContainerName := strings.TrimSpace(ds.ContainerName)
	if trimmedContainerName == "" {
		return fmt.Errorf("container name is required")
	}
	if strings.TrimSpace(ds.Image) == "" {
		return fmt.Errorf("container image is required")
	}
	if strings.TrimSpace(ds.Veth) == "" {
		return fmt.Errorf("veth interface is required")
	}
	if strings.TrimSpace(ds.VethGateway) == "" {
		return fmt.Errorf("veth gateway is required")
	}
	if strings.TrimSpace(ds.Token) == "" {
		return fmt.Errorf("token is required")
	}
	if strings.TrimSpace(ds.OverlayIP) == "" {
		return fmt.Errorf("overlay IP is required")
	}
	if strings.TrimSpace(ds.OverlayNet) == "" {
		return fmt.Errorf("overlay network is required")
	}
	if ds.ListenPort < 0 || ds.ListenPort > 65535 {
		return fmt.Errorf("listen port %d is out of range", ds.ListenPort)
	}
	if len(ds.Masters) == 0 {
		return fmt.Errorf("at least one master is required")
	}

	if _, err := netip.ParseAddr(ds.OverlayIP); err != nil {
		return fmt.Errorf("invalid overlay IP %q: %w", ds.OverlayIP, err)
	}
	if _, err := netip.ParsePrefix(ds.OverlayNet); err != nil {
		return fmt.Errorf("invalid overlay network %q: %w", ds.OverlayNet, err)
	}
	if _, err := netip.ParseAddr(ds.VethGateway); err != nil {
		if _, prefixErr := netip.ParsePrefix(ds.VethGateway); prefixErr != nil {
			return fmt.Errorf("invalid veth gateway %q: %v", ds.VethGateway, prefixErr)
		}
	}

	for _, master := range ds.Masters {
		if strings.TrimSpace(master) == "" {
			return fmt.Errorf("masters list contains empty entry")
		}
	}

	return nil
}

func buildDeployEnvVars(ds DeployScript) map[string]string {
	envVars := map[string]string{
		"MESH_TOKEN":      strings.TrimSpace(ds.Token),
		"MESH_MODE":       "client",
		"MESH_OVERLAY_IP": strings.TrimSpace(ds.OverlayIP),
		"MESH_MASTERS":    strings.Join(ds.Masters, ","),
	}

	if ds.ListenPort > 0 {
		envVars["MESH_LISTEN_PORT"] = fmt.Sprintf("%d", ds.ListenPort)
	}

	if strings.TrimSpace(ds.AWGConfig) != "" {
		envVars["MESH_AWG_CONFIG"] = ds.AWGConfig
	}

	return envVars
}

func validateRotateParams(params RotateParams) error {
	values := []struct {
		name  string
		value int
	}{
		{name: "jc", value: params.Jc},
		{name: "jmin", value: params.Jmin},
		{name: "jmax", value: params.Jmax},
		{name: "s1", value: params.S1},
		{name: "s2", value: params.S2},
		{name: "h1", value: params.H1},
		{name: "h2", value: params.H2},
		{name: "h3", value: params.H3},
		{name: "h4", value: params.H4},
	}

	for _, current := range values {
		if current.value < 0 {
			return fmt.Errorf("%s must be >= 0", current.name)
		}
	}
	if params.Jmin > params.Jmax {
		return fmt.Errorf("jmin (%d) must be <= jmax (%d)", params.Jmin, params.Jmax)
	}

	return nil
}

func executeTemplate(name, source string, data any) (string, error) {
	compiledTemplate, err := template.New(name).Option("missingkey=error").Parse(source)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}

	var output strings.Builder
	if err := compiledTemplate.Execute(&output, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}

	return output.String(), nil
}
