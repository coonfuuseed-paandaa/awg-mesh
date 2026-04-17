//go:build linux && !nocapture

package awggen

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
)

const (
	defaultCaptureTimeout     = 10 * time.Second
	defaultCountPerDomain     = 3
	defaultHandshakeDialDelay = 5 * time.Second
)

// CaptureConfig configures a TLS/QUIC packet capture session.
type CaptureConfig struct {
	Interface      string
	Domains        []string
	CountPerDomain int
	Timeout        time.Duration
}

// CaptureResult holds a single captured packet record.
type CaptureResult struct {
	Domain    string
	Protocol  string
	Data      []byte
	Timestamp time.Time
}

// Capture runs live packet capture and returns TLS/QUIC payloads for the
// requested domains.
func Capture(cfg CaptureConfig) ([]CaptureResult, error) {
	if cfg.Interface == "" {
		return nil, errors.New("capture: interface is required")
	}
	if cfg.CountPerDomain < 0 {
		return nil, errors.New("capture: count_per_domain must be non-negative")
	}
	if len(cfg.Domains) == 0 {
		return []CaptureResult{}, nil
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultCaptureTimeout
	}
	countPerDomain := cfg.CountPerDomain
	if countPerDomain == 0 {
		countPerDomain = defaultCountPerDomain
	}

	ipToDomain := resolveDomainsToIPs(cfg.Domains)
	if len(ipToDomain) == 0 {
		return []CaptureResult{}, nil
	}

	handle, err := pcap.OpenLive(cfg.Interface, 65536, true, timeout)
	if err != nil {
		return nil, fmt.Errorf("capture: open pcap: %w", err)
	}
	defer handle.Close()

	if err := handle.SetBPFFilter(buildCaptureFilter(ipToDomain)); err != nil {
		return nil, fmt.Errorf("capture: set BPF filter: %w", err)
	}

	go dialCaptureTargets(cfg.Domains, defaultHandshakeDialDelay)

	deadline := time.Now().Add(timeout)
	domainCount := make(map[string]int)
	results := make([]CaptureResult, 0)
	packetSource := gopacket.NewPacketSource(handle, handle.LinkType())

	for packet := range packetSource.Packets() {
		if time.Now().After(deadline) {
			break
		}
		result, matched := captureResultFromPacket(packet, ipToDomain, domainCount, countPerDomain)
		if !matched {
			continue
		}
		results = append(results, result)
	}

	return results, nil
}

func captureResultFromPacket(
	packet gopacket.Packet,
	ipToDomain map[string]string,
	domainCount map[string]int,
	countPerDomain int,
) (CaptureResult, bool) {
	networkLayer := packet.NetworkLayer()
	if networkLayer == nil {
		return CaptureResult{}, false
	}
	domain, ok := ipToDomain[networkLayer.NetworkFlow().Dst().String()]
	if !ok {
		return CaptureResult{}, false
	}
	if domainCount[domain] >= countPerDomain {
		return CaptureResult{}, false
	}

	if data, err := ExtractTLSClientHello(packet); err == nil {
		domainCount[domain]++
		return CaptureResult{
			Domain:    domain,
			Protocol:  "tls",
			Data:      append([]byte(nil), data...),
			Timestamp: packet.Metadata().Timestamp,
		}, true
	}
	if data, err := ExtractQUICInitial(packet); err == nil {
		domainCount[domain]++
		return CaptureResult{
			Domain:    domain,
			Protocol:  "quic",
			Data:      append([]byte(nil), data...),
			Timestamp: packet.Metadata().Timestamp,
		}, true
	}
	return CaptureResult{}, false
}

func resolveDomainsToIPs(domains []string) map[string]string {
	ipToDomain := make(map[string]string)
	seenDomains := make(map[string]struct{}, len(domains))

	for _, domain := range domains {
		trimmedDomain := strings.TrimSpace(domain)
		if trimmedDomain == "" {
			continue
		}
		if _, alreadyAdded := seenDomains[trimmedDomain]; alreadyAdded {
			continue
		}
		seenDomains[trimmedDomain] = struct{}{}

		addresses, err := net.LookupIP(trimmedDomain)
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ipToDomain[address.String()] = trimmedDomain
		}
	}

	return ipToDomain
}

func buildCaptureFilter(ipToDomain map[string]string) string {
	parts := make([]string, 0, len(ipToDomain))
	for ip := range ipToDomain {
		parts = append(parts, fmt.Sprintf("host %s", ip))
	}
	if len(parts) == 0 {
		return "tcp port 443 or udp port 443"
	}
	return fmt.Sprintf("(%s) and (tcp port 443 or udp port 443)", strings.Join(parts, " or "))
}

func dialCaptureTargets(domains []string, timeout time.Duration) {
	var wg sync.WaitGroup
	for _, domain := range domains {
		trimmedDomain := strings.TrimSpace(domain)
		if trimmedDomain == "" {
			continue
		}
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			dialAndPrimeTLSSessions(host, timeout)
		}(trimmedDomain)
	}
	wg.Wait()
}

// dialAndPrimeTLSSessions forces a real TLS ClientHello onto the wire so the
// pcap loop can observe the handshake bytes. The previous implementation
// used net.DialTimeout for TCP, which completes the 3-way handshake but never
// emits a TLS record — capture then matched zero packets because every frame
// on the wire after SYN/ACK was either FIN or a stray server RST.
//
// We ignore the TLS error: certificate mismatches, expired certs, or server-
// side rejections are all irrelevant here. We only need the ClientHello bytes
// to hit the network before the socket is closed. InsecureSkipVerify is
// intentional for the same reason.
//
// The UDP/QUIC branch was a 1-byte write that real QUIC stacks silently drop.
// A future revision should craft a real QUIC Initial packet (ideally via a
// quic-go client). For now we stop sending a spurious byte and leave QUIC
// fingerprinting as a known gap — see the TODO below.
func dialAndPrimeTLSSessions(domain string, timeout time.Duration) {
	serverAddr := net.JoinHostPort(domain, "443")
	dialer := &net.Dialer{Timeout: timeout}
	// MinVersion is documentation — with InsecureSkipVerify=true the server
	// certificate is never validated and the cipher/version selection is
	// purely the server's decision. The field is kept as a guard against a
	// future patch that drops InsecureSkipVerify without re-thinking security.
	tlsCfg := &tls.Config{
		ServerName:         domain,
		InsecureSkipVerify: true, //nolint:gosec // we only want the ClientHello emitted on the wire
		MinVersion:         tls.VersionTLS12,
	}
	if conn, err := tls.DialWithDialer(dialer, "tcp", serverAddr, tlsCfg); err == nil {
		_ = conn.Close()
	}
	// TODO: craft a real QUIC Initial packet for UDP/443 capture. The old
	// `conn.Write([]byte{0xc0})` was a no-op against real QUIC servers.
}

func ExtractTLSClientHello(packet gopacket.Packet) ([]byte, error) {
	tcpLayer := packet.Layer(layers.LayerTypeTCP)
	if tcpLayer == nil {
		return nil, errors.New("not a TCP packet")
	}
	tcp := tcpLayer.(*layers.TCP)
	payload := tcp.Payload
	if len(payload) < 6 {
		return nil, errors.New("TLS payload too short")
	}
	if payload[0] != 0x16 {
		return nil, errors.New("not a TLS handshake record")
	}
	if payload[5] != 0x01 {
		return nil, errors.New("not a TLS ClientHello")
	}
	return payload, nil
}

func ExtractQUICInitial(packet gopacket.Packet) ([]byte, error) {
	udpLayer := packet.Layer(layers.LayerTypeUDP)
	if udpLayer == nil {
		return nil, errors.New("not a UDP packet")
	}
	udp := udpLayer.(*layers.UDP)
	payload := udp.Payload
	if len(payload) == 0 {
		return nil, errors.New("UDP payload too short")
	}
	if payload[0]&0x80 == 0 {
		return nil, errors.New("not a QUIC long header packet")
	}
	if payload[0]&0xf0 != 0xc0 {
		return nil, errors.New("not a QUIC Initial packet")
	}
	return payload, nil
}
