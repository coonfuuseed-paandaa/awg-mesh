package ingress

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var (
	ErrClientHelloTooShort = errors.New("tls client hello is too short")
	ErrClientHelloNoSNI    = errors.New("tls client hello does not contain SNI")
)

// Classifier maps TLS ClientHello SNI hostnames to ingress routes.
type Classifier struct {
	registry *Registry
}

func NewClassifier(registry *Registry) *Classifier {
	return &Classifier{registry: registry}
}

func (c *Classifier) ClassifyHostname(hostname string) (Route, bool) {
	if c == nil || c.registry == nil {
		return Route{}, false
	}
	return c.registry.Lookup(hostname)
}

func (c *Classifier) ClassifyClientHello(record []byte) (Route, error) {
	hostname, err := ClientHelloServerName(record)
	if err != nil {
		return Route{}, err
	}
	route, ok := c.ClassifyHostname(hostname)
	if !ok {
		return Route{}, fmt.Errorf("hostname %q is not allowed", hostname)
	}
	return route, nil
}

// ClientHelloServerName extracts the SNI hostname from one TLS ClientHello record.
func ClientHelloServerName(record []byte) (string, error) {
	if len(record) < 5 {
		return "", ErrClientHelloTooShort
	}
	if record[0] != 22 {
		return "", fmt.Errorf("tls record content type %d is not handshake", record[0])
	}
	recordLen := int(binary.BigEndian.Uint16(record[3:5]))
	if len(record)-5 < recordLen {
		return "", ErrClientHelloTooShort
	}
	body := record[5 : 5+recordLen]
	if len(body) < 42 {
		return "", ErrClientHelloTooShort
	}
	if body[0] != 1 {
		return "", fmt.Errorf("tls handshake is not client hello")
	}
	helloLen := int(body[1])<<16 | int(body[2])<<8 | int(body[3])
	if len(body)-4 < helloLen {
		return "", ErrClientHelloTooShort
	}
	cursor := 4 + 2 + 32
	if cursor >= len(body) {
		return "", ErrClientHelloTooShort
	}
	sessionIDLen := int(body[cursor])
	cursor++
	cursor += sessionIDLen
	if cursor+2 > len(body) {
		return "", ErrClientHelloTooShort
	}
	cipherSuitesLen := int(binary.BigEndian.Uint16(body[cursor : cursor+2]))
	cursor += 2 + cipherSuitesLen
	if cursor >= len(body) {
		return "", ErrClientHelloTooShort
	}
	compressionMethodsLen := int(body[cursor])
	cursor++
	cursor += compressionMethodsLen
	if cursor+2 > len(body) {
		return "", ErrClientHelloNoSNI
	}
	extensionsLen := int(binary.BigEndian.Uint16(body[cursor : cursor+2]))
	cursor += 2
	if cursor+extensionsLen > len(body) {
		return "", ErrClientHelloTooShort
	}
	extensionsEnd := cursor + extensionsLen
	for cursor+4 <= extensionsEnd {
		extType := binary.BigEndian.Uint16(body[cursor : cursor+2])
		extLen := int(binary.BigEndian.Uint16(body[cursor+2 : cursor+4]))
		cursor += 4
		if cursor+extLen > extensionsEnd {
			return "", ErrClientHelloTooShort
		}
		if extType == 0 {
			return parseSNIServerName(body[cursor : cursor+extLen])
		}
		cursor += extLen
	}
	return "", ErrClientHelloNoSNI
}

func parseSNIServerName(data []byte) (string, error) {
	if len(data) < 2 {
		return "", ErrClientHelloTooShort
	}
	listLen := int(binary.BigEndian.Uint16(data[:2]))
	if listLen == 0 || len(data)-2 < listLen {
		return "", ErrClientHelloTooShort
	}
	cursor := 2
	end := cursor + listLen
	for cursor+3 <= end {
		nameType := data[cursor]
		nameLen := int(binary.BigEndian.Uint16(data[cursor+1 : cursor+3]))
		cursor += 3
		if cursor+nameLen > end {
			return "", ErrClientHelloTooShort
		}
		if nameType == 0 {
			return normalizeHostname(string(data[cursor : cursor+nameLen]))
		}
		cursor += nameLen
	}
	return "", ErrClientHelloNoSNI
}
