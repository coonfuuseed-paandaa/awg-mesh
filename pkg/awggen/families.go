package awggen

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// ProtocolFamily defines an I-spec template family.
type ProtocolFamily struct {
	Name        string
	Description string
	I1          string
	I2          string
	I3          string
	I4          string
	I5          string
}

var protocolFamilies = []ProtocolFamily{
	{
		Name:        "TLSClientHello",
		Description: "TLS 1.2/1.3 client hello fingerprint with common extension blocks",
		I1:          "<b 160301> <r 2> <b 0100> <r 4> <b 0303> <rc 32>",
		I2:          "<b 00> <r 32> <b 00>",
		I3:          "<b 001e> <b 130113021303c02fc02bc030c02c009f009ecca9cca8>",
		I4:          "<t TLS_EXTENSIONS>",
		I5:          "<rd 4 8>",
	},
	{
		Name:        "TLSServerHello",
		Description: "TLS server hello fingerprint with negotiated suite and extensions",
		I1:          "<b 160303> <r 2> <b 0200> <r 4> <b 0303> <rc 32>",
		I2:          "<b 00> <r 32> <b 1301>",
		I3:          "<b 00> <b 0000>",
		I4:          "<t TLS_SERVER_EXTENSIONS>",
		I5:          "<rd 3 6>",
	},
	{
		Name:        "QUICInitial",
		Description: "QUIC v1 initial packet style with transport parameter block",
		I1:          "<t QUIC_INITIAL>",
		I2:          "<b c3> <rc 8> <b 08>",
		I3:          "<b 00> <r 20>",
		I4:          "<t QUIC_TRANSPORT_PARAMS>",
		I5:          "<rd 6 10>",
	},
	{
		Name:        "DNSQuery",
		Description: "UDP DNS query framing with random transaction identifiers",
		I1:          "<t DNS_QUERY>",
		I2:          "<rc 2> <b 0100> <b 0001> <b 0000> <b 0000> <b 0000>",
		I3:          "<b 076578616d706c65> <b 03636f6d00>",
		I4:          "<b 0001> <b 0001>",
		I5:          "<rd 4 6>",
	},
	{
		Name:        "DNSResponse",
		Description: "UDP DNS response framing with answer section",
		I1:          "<t DNS_RESPONSE>",
		I2:          "<rc 2> <b 8180> <b 0001> <b 0001> <b 0000> <b 0000>",
		I3:          "<b 076578616d706c65> <b 03636f6d00> <b 0001> <b 0001>",
		I4:          "<b c00c> <b 0001> <b 0001> <b 0000012c> <b 0004> <r 4>",
		I5:          "<rd 4 6>",
	},
	{
		Name:        "HTTPGet",
		Description: "HTTP/1.1 GET style request framing",
		I1:          "<t HTTP_GET>",
		I2:          "<b 486f73743a206578616d706c652e636f6d>",
		I3:          "<b 557365722d4167656e743a204d6f7a696c6c612f352e30>",
		I4:          "<b 4163636570743a202a2f2a> <b 0d0a>",
		I5:          "<b 0d0a>",
	},
	{
		Name:        "DTLSClientHello",
		Description: "DTLS client hello framing over datagram transport",
		I1:          "<t DTLS_CLIENT_HELLO>",
		I2:          "<b 16fefd> <r 2> <b 0100> <r 4> <b fefd> <rc 32>",
		I3:          "<b 00> <r 32> <b 00>",
		I4:          "<t DTLS_EXTENSIONS>",
		I5:          "<rd 4 8>",
	},
	{
		Name:        "STUNBinding",
		Description: "STUN binding request with ICE-style attributes",
		I1:          "<t STUN_BINDING>",
		I2:          "<b 00010008> <b 2112a442> <rc 12>",
		I3:          "<b 80220010> <rc 16>",
		I4:          "<b 00060006> <b 757365720000>",
		I5:          "<rd 5 9>",
	},
	{
		Name:        "NTPQuery",
		Description: "NTP client query packet pattern",
		I1:          "<t NTP_QUERY>",
		I2:          "<b 1b> <b 00000000> <b 00000000>",
		I3:          "<b 00000000> <b 00000000>",
		I4:          "<rc 8>",
		I5:          "<rd 2 4>",
	},
}

// GetFamily returns a named protocol family.
func GetFamily(name string) (*ProtocolFamily, error) {
	normalized := strings.TrimSpace(name)
	if normalized == "" {
		return nil, fmt.Errorf("family name is required")
	}

	for _, family := range protocolFamilies {
		if strings.EqualFold(family.Name, normalized) {
			selected := family
			return &selected, nil
		}
	}

	return nil, fmt.Errorf("unknown protocol family: %q", name)
}

// ListFamilies returns all available protocol families.
func ListFamilies() []ProtocolFamily {
	result := make([]ProtocolFamily, len(protocolFamilies))
	copy(result, protocolFamilies)
	return result
}

// RandomFamily returns a randomly selected protocol family.
func RandomFamily() *ProtocolFamily {
	if len(protocolFamilies) == 0 {
		return nil
	}

	selected := protocolFamilies[rand.IntN(len(protocolFamilies))]
	return &selected
}
