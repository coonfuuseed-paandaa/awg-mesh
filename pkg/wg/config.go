package wg

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// GenerateConfig renders an INI-style WireGuard/AWG config.
func GenerateConfig(cfg Config, peers []PeerConfig) string {
	var builder strings.Builder
	builder.WriteString("[Interface]\n")

	if cfg.PrivateKey != nil {
		builder.WriteString("PrivateKey = " + cfg.PrivateKey.String() + "\n")
	}
	if cfg.ListenPort != nil {
		builder.WriteString("ListenPort = " + strconv.Itoa(*cfg.ListenPort) + "\n")
	}

	appendIntSetting(&builder, "Jc", cfg.Jc)
	appendIntSetting(&builder, "Jmin", cfg.Jmin)
	appendIntSetting(&builder, "Jmax", cfg.Jmax)
	appendIntSetting(&builder, "S1", cfg.S1)
	appendIntSetting(&builder, "S2", cfg.S2)
	appendIntSetting(&builder, "S3", cfg.S3)
	appendIntSetting(&builder, "S4", cfg.S4)

	appendStringSetting(&builder, "H1", cfg.H1)
	appendStringSetting(&builder, "H2", cfg.H2)
	appendStringSetting(&builder, "H3", cfg.H3)
	appendStringSetting(&builder, "H4", cfg.H4)
	appendStringSetting(&builder, "I1", cfg.I1)
	appendStringSetting(&builder, "I2", cfg.I2)
	appendStringSetting(&builder, "I3", cfg.I3)
	appendStringSetting(&builder, "I4", cfg.I4)
	appendStringSetting(&builder, "I5", cfg.I5)

	for _, peer := range peers {
		builder.WriteString("\n[Peer]\n")
		builder.WriteString("PublicKey = " + peer.PublicKey.String() + "\n")

		if peer.PresharedKey != nil {
			builder.WriteString("PresharedKey = " + peer.PresharedKey.String() + "\n")
		}
		if peer.Endpoint != nil {
			builder.WriteString("Endpoint = " + peer.Endpoint.String() + "\n")
		}
		if peer.PersistentKeepaliveInterval != nil {
			seconds := int(peer.PersistentKeepaliveInterval.Seconds())
			builder.WriteString("PersistentKeepalive = " + strconv.Itoa(seconds) + "\n")
		}
		if len(peer.AllowedIPs) > 0 {
			allowedIPs := make([]string, 0, len(peer.AllowedIPs))
			for _, allowedIP := range peer.AllowedIPs {
				allowedIPs = append(allowedIPs, allowedIP.String())
			}
			builder.WriteString("AllowedIPs = " + strings.Join(allowedIPs, ", ") + "\n")
		}
	}

	return builder.String()
}

// ParseConfigFile parses an INI-style WireGuard/AWG config file.
func ParseConfigFile(path string) (*Config, []PeerConfig, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil, fmt.Errorf("config path is required")
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open config file: %w", err)
	}
	defer file.Close()

	cfg := &Config{}
	peers := make([]PeerConfig, 0)

	currentSection := ""
	var currentPeer *PeerConfig

	flushPeer := func() {
		if currentPeer == nil {
			return
		}
		peers = append(peers, *currentPeer)
		currentPeer = nil
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			if section != "peer" {
				flushPeer()
			}

			currentSection = section
			if section == "peer" {
				flushPeer()
				currentPeer = &PeerConfig{}
			}
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, nil, fmt.Errorf("invalid config line: %q", line)
		}

		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch currentSection {
		case "interface":
			if err := parseInterfaceKV(cfg, key, value); err != nil {
				return nil, nil, err
			}
		case "peer":
			if currentPeer == nil {
				return nil, nil, fmt.Errorf("peer key outside peer section: %s", key)
			}
			if err := parsePeerKV(currentPeer, key, value); err != nil {
				return nil, nil, err
			}
		default:
			return nil, nil, fmt.Errorf("key %q outside known section", key)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan config file: %w", err)
	}

	flushPeer()
	cfg.Peers = append([]PeerConfig(nil), peers...)
	return cfg, peers, nil
}

func appendIntSetting(builder *strings.Builder, key string, value *int) {
	if value == nil {
		return
	}
	builder.WriteString(key + " = " + strconv.Itoa(*value) + "\n")
}

func appendStringSetting(builder *strings.Builder, key string, value *string) {
	if value == nil {
		return
	}
	builder.WriteString(key + " = " + *value + "\n")
}

func parseInterfaceKV(cfg *Config, key string, value string) error {
	switch key {
	case "privatekey":
		parsed, err := ParseKey(value)
		if err != nil {
			return fmt.Errorf("parse Interface PrivateKey: %w", err)
		}
		cfg.PrivateKey = &parsed
	case "listenport":
		port, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("parse Interface ListenPort: %w", err)
		}
		cfg.ListenPort = &port
	case "fwmark":
		mark, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("parse Interface FwMark: %w", err)
		}
		cfg.FirewallMark = &mark
	case "jc":
		return parseIntField(value, "Interface Jc", &cfg.Jc)
	case "jmin":
		return parseIntField(value, "Interface Jmin", &cfg.Jmin)
	case "jmax":
		return parseIntField(value, "Interface Jmax", &cfg.Jmax)
	case "s1":
		return parseIntField(value, "Interface S1", &cfg.S1)
	case "s2":
		return parseIntField(value, "Interface S2", &cfg.S2)
	case "s3":
		return parseIntField(value, "Interface S3", &cfg.S3)
	case "s4":
		return parseIntField(value, "Interface S4", &cfg.S4)
	case "h1":
		cfg.H1 = StrPtr(value)
	case "h2":
		cfg.H2 = StrPtr(value)
	case "h3":
		cfg.H3 = StrPtr(value)
	case "h4":
		cfg.H4 = StrPtr(value)
	case "i1":
		cfg.I1 = StrPtr(value)
	case "i2":
		cfg.I2 = StrPtr(value)
	case "i3":
		cfg.I3 = StrPtr(value)
	case "i4":
		cfg.I4 = StrPtr(value)
	case "i5":
		cfg.I5 = StrPtr(value)
	}
	return nil
}

func parsePeerKV(peer *PeerConfig, key string, value string) error {
	switch key {
	case "publickey":
		parsed, err := ParseKey(value)
		if err != nil {
			return fmt.Errorf("parse Peer PublicKey: %w", err)
		}
		peer.PublicKey = parsed
	case "presharedkey":
		parsed, err := ParseKey(value)
		if err != nil {
			return fmt.Errorf("parse Peer PresharedKey: %w", err)
		}
		peer.PresharedKey = &parsed
	case "endpoint":
		endpoint, err := net.ResolveUDPAddr("udp", value)
		if err != nil {
			return fmt.Errorf("parse Peer Endpoint: %w", err)
		}
		peer.Endpoint = endpoint
	case "persistentkeepalive":
		seconds, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("parse Peer PersistentKeepalive: %w", err)
		}
		interval := time.Duration(seconds) * time.Second
		peer.PersistentKeepaliveInterval = &interval
	case "allowedips":
		cidrs := strings.Split(value, ",")
		allowedIPs := make([]net.IPNet, 0, len(cidrs))
		for _, cidr := range cidrs {
			trimmed := strings.TrimSpace(cidr)
			if trimmed == "" {
				continue
			}
			_, ipNet, err := net.ParseCIDR(trimmed)
			if err != nil {
				return fmt.Errorf("parse Peer AllowedIPs item %q: %w", trimmed, err)
			}
			allowedIPs = append(allowedIPs, *ipNet)
		}
		peer.AllowedIPs = append([]net.IPNet(nil), allowedIPs...)
	}
	return nil
}

func parseIntField(value string, fieldName string, target **int) error {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("parse %s: %w", fieldName, err)
	}
	*target = &parsed
	return nil
}
