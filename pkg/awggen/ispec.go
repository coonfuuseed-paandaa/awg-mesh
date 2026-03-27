package awggen

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"unicode"
)

const maxTemplateExpansionDepth = 8

const (
	iSpecKindLiteral = "literal"
	iSpecKindBinary  = "binary"
	iSpecKindRandom  = "random"
	iSpecKindCRandom = "crypto-random"
	iSpecKindDigits  = "digits"
	iSpecKindType    = "type"
)

type iSpecOperation struct {
	Kind         string
	Literal      string
	Binary       []byte
	Length       int
	MinLength    int
	MaxLength    int
	TemplateType string
}

var protocolTemplateExpansions = map[string]string{
	"TLS_CLIENT_HELLO":      "<b 160301> <r 2> <b 0100> <r 4> <b 0303> <rc 32>",
	"TLS_SERVER_HELLO":      "<b 160303> <r 2> <b 0200> <r 4> <b 0303> <rc 32>",
	"TLS_EXTENSIONS":        "<b 000a0006001700180019> <b 000d000400020403> <b 002b0003020304>",
	"TLS_SERVER_EXTENSIONS": "<b 002b00020304> <b 00330002001d>",
	"QUIC_INITIAL":          "<b c30000000108> <rc 8> <b 00> <r 20>",
	"QUIC_TRANSPORT_PARAMS": "<b 00390020> <rc 32>",
	"DNS_QUERY":             "<rc 2> <b 01000001000000000000>",
	"DNS_RESPONSE":          "<rc 2> <b 81800001000100000000>",
	"HTTP_GET":              "<b 474554202f20485454502f312e31> <b 0d0a>",
	"DTLS_CLIENT_HELLO":     "<b 16fefd> <r 2> <b 0100> <r 4> <b fefd> <rc 32>",
	"DTLS_EXTENSIONS":       "<b 000f000101> <b 000a00080006001700180019>",
	"STUN_BINDING":          "<b 00010000> <b 2112a442> <rc 12>",
	"NTP_QUERY":             "<b 1b00000000000000000000000000000000000000000000000000000000000000>",
}

// ParseISpec parses an I-spec template into concrete bytes.
func ParseISpec(spec string) ([]byte, error) {
	ops, err := parseISpecOperations(spec)
	if err != nil {
		return nil, err
	}

	return executeISpecOperations(ops, 0, make(map[string]struct{}))
}

// GenerateISpec returns a family I-spec template set as raw strings.
func GenerateISpec(family *ProtocolFamily) ([5]string, error) {
	var values [5]string
	if family == nil {
		return values, fmt.Errorf("protocol family is required")
	}

	values = [5]string{family.I1, family.I2, family.I3, family.I4, family.I5}
	for index, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if err := ValidateISpec(value); err != nil {
			return [5]string{}, fmt.Errorf("invalid I%d template: %w", index+1, err)
		}
	}

	return values, nil
}

// ValidateISpec validates I-spec syntax and template references.
func ValidateISpec(spec string) error {
	return validateISpecRecursive(spec, 0, make(map[string]struct{}))
}

func validateISpecRecursive(spec string, depth int, stack map[string]struct{}) error {
	if depth > maxTemplateExpansionDepth {
		return fmt.Errorf("template expansion depth exceeded")
	}

	ops, err := parseISpecOperations(spec)
	if err != nil {
		return err
	}

	for _, op := range ops {
		if op.Kind != iSpecKindType {
			continue
		}
		if _, exists := stack[op.TemplateType]; exists {
			return fmt.Errorf("recursive template reference: %s", op.TemplateType)
		}

		template, exists := protocolTemplateExpansions[op.TemplateType]
		if !exists {
			return fmt.Errorf("unknown protocol template: %s", op.TemplateType)
		}

		stack[op.TemplateType] = struct{}{}
		if err := validateISpecRecursive(template, depth+1, stack); err != nil {
			delete(stack, op.TemplateType)
			return err
		}
		delete(stack, op.TemplateType)
	}

	return nil
}

func parseISpecOperations(spec string) ([]iSpecOperation, error) {
	tokens, err := tokenizeISpec(spec)
	if err != nil {
		return nil, err
	}

	ops := make([]iSpecOperation, 0, len(tokens))
	for _, token := range tokens {
		op, err := parseISpecToken(token)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}

	return ops, nil
}

func tokenizeISpec(spec string) ([]string, error) {
	tokens := make([]string, 0)
	for cursor := 0; cursor < len(spec); {
		for cursor < len(spec) && unicode.IsSpace(rune(spec[cursor])) {
			cursor++
		}
		if cursor >= len(spec) {
			break
		}

		if spec[cursor] == '<' {
			offset := strings.IndexByte(spec[cursor:], '>')
			if offset < 0 {
				return nil, fmt.Errorf("unterminated tag at position %d", cursor)
			}
			tokens = append(tokens, spec[cursor:cursor+offset+1])
			cursor += offset + 1
			continue
		}

		end := cursor
		for end < len(spec) && !unicode.IsSpace(rune(spec[end])) {
			if spec[end] == '<' || spec[end] == '>' {
				return nil, fmt.Errorf("invalid raw token character %q at position %d", spec[end], end)
			}
			end++
		}
		tokens = append(tokens, spec[cursor:end])
		cursor = end
	}

	return tokens, nil
}

func parseISpecToken(token string) (iSpecOperation, error) {
	if strings.HasPrefix(token, "<") && strings.HasSuffix(token, ">") {
		return parseISpecTag(token)
	}

	return iSpecOperation{Kind: iSpecKindLiteral, Literal: token}, nil
}

func parseISpecTag(token string) (iSpecOperation, error) {
	inner := strings.TrimSpace(token[1 : len(token)-1])
	parts := strings.Fields(inner)
	if len(parts) == 0 {
		return iSpecOperation{}, fmt.Errorf("empty tag is not allowed")
	}

	switch parts[0] {
	case "b":
		return parseBinaryTag(parts)
	case "r":
		return parseRandomTag(parts, iSpecKindRandom)
	case "rc":
		return parseRandomTag(parts, iSpecKindCRandom)
	case "rd":
		return parseRandomDigitsTag(parts)
	case "t":
		return parseTemplateTag(parts)
	default:
		return iSpecOperation{}, fmt.Errorf("unsupported I-spec tag: %q", parts[0])
	}
}

func parseBinaryTag(parts []string) (iSpecOperation, error) {
	if len(parts) != 2 {
		return iSpecOperation{}, fmt.Errorf("<b> expects one argument")
	}

	binary, err := hex.DecodeString(parts[1])
	if err != nil {
		return iSpecOperation{}, fmt.Errorf("invalid hex in <b>: %w", err)
	}

	return iSpecOperation{Kind: iSpecKindBinary, Binary: binary}, nil
}

func parseRandomTag(parts []string, kind string) (iSpecOperation, error) {
	if len(parts) != 2 {
		return iSpecOperation{}, fmt.Errorf("<%s> expects one numeric argument", parts[0])
	}

	length, err := parseISpecLength(parts[1], "length")
	if err != nil {
		return iSpecOperation{}, fmt.Errorf("invalid <%s> length: %w", parts[0], err)
	}

	return iSpecOperation{Kind: kind, Length: length}, nil
}

func parseRandomDigitsTag(parts []string) (iSpecOperation, error) {
	if len(parts) != 3 {
		return iSpecOperation{}, fmt.Errorf("<rd> expects min and max lengths")
	}

	minLength, err := parseISpecLength(parts[1], "min length")
	if err != nil {
		return iSpecOperation{}, fmt.Errorf("invalid <rd> min length: %w", err)
	}
	maxLength, err := parseISpecLength(parts[2], "max length")
	if err != nil {
		return iSpecOperation{}, fmt.Errorf("invalid <rd> max length: %w", err)
	}
	if maxLength < minLength {
		return iSpecOperation{}, fmt.Errorf("<rd> max length must be >= min length")
	}

	return iSpecOperation{Kind: iSpecKindDigits, MinLength: minLength, MaxLength: maxLength}, nil
}

func parseTemplateTag(parts []string) (iSpecOperation, error) {
	if len(parts) != 2 {
		return iSpecOperation{}, fmt.Errorf("<t> expects one template name")
	}

	templateType := strings.ToUpper(parts[1])
	if _, exists := protocolTemplateExpansions[templateType]; !exists {
		return iSpecOperation{}, fmt.Errorf("unknown protocol template: %s", templateType)
	}

	return iSpecOperation{Kind: iSpecKindType, TemplateType: templateType}, nil
}

func parseISpecLength(value, fieldName string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", fieldName)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s must be >= 0", fieldName)
	}

	return parsed, nil
}

func executeISpecOperations(ops []iSpecOperation, depth int, stack map[string]struct{}) ([]byte, error) {
	if depth > maxTemplateExpansionDepth {
		return nil, fmt.Errorf("template expansion depth exceeded")
	}

	output := make([]byte, 0)
	for _, op := range ops {
		payload, err := executeISpecOperation(op, depth, stack)
		if err != nil {
			return nil, err
		}
		output = append(output, payload...)
	}

	return output, nil
}

func executeISpecOperation(op iSpecOperation, depth int, stack map[string]struct{}) ([]byte, error) {
	switch op.Kind {
	case iSpecKindLiteral:
		return []byte(op.Literal), nil
	case iSpecKindBinary:
		copied := make([]byte, len(op.Binary))
		copy(copied, op.Binary)
		return copied, nil
	case iSpecKindRandom:
		return randomBytes(op.Length), nil
	case iSpecKindCRandom:
		return cryptoRandomBytes(op.Length)
	case iSpecKindDigits:
		return randomDigitBytes(op.MinLength, op.MaxLength), nil
	case iSpecKindType:
		return expandProtocolTemplate(op.TemplateType, depth+1, stack)
	default:
		return nil, fmt.Errorf("unsupported operation kind: %s", op.Kind)
	}
}

func expandProtocolTemplate(templateType string, depth int, stack map[string]struct{}) ([]byte, error) {
	if _, exists := stack[templateType]; exists {
		return nil, fmt.Errorf("recursive template reference: %s", templateType)
	}

	templateSpec, exists := protocolTemplateExpansions[templateType]
	if !exists {
		return nil, fmt.Errorf("unknown protocol template: %s", templateType)
	}

	stack[templateType] = struct{}{}
	defer delete(stack, templateType)

	ops, err := parseISpecOperations(templateSpec)
	if err != nil {
		return nil, fmt.Errorf("invalid template %s: %w", templateType, err)
	}

	return executeISpecOperations(ops, depth, stack)
}

func randomBytes(length int) []byte {
	payload := make([]byte, length)
	for index := 0; index < length; index++ {
		payload[index] = byte(rand.IntN(256))
	}
	return payload
}

func cryptoRandomBytes(length int) ([]byte, error) {
	payload := make([]byte, length)
	if length == 0 {
		return payload, nil
	}

	if _, err := crand.Read(payload); err != nil {
		return nil, fmt.Errorf("crypto random read failed: %w", err)
	}

	return payload, nil
}

func randomDigitBytes(minLength, maxLength int) []byte {
	length := minLength + rand.IntN(maxLength-minLength+1)
	payload := make([]byte, length)
	for index := 0; index < length; index++ {
		payload[index] = byte('0' + rand.IntN(10))
	}
	return payload
}
