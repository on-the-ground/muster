package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/on-the-ground/muster/internal/strictjson"
)

// Schema is a validated definition in muster's closed JSON Schema profile.
type Schema struct{ root map[string]any }

// CompileSchema refuses unsupported keywords instead of silently ignoring them
// or rounding numbers inside an upstream schema representation.
func CompileSchema(raw json.RawMessage, output bool) (*Schema, error) {
	m, err := strictjson.Object(raw)
	if err != nil {
		return nil, err
	}
	if m["type"] != "object" {
		return nil, errors.New("root schema must have type object")
	}
	if err := schemaNode(m, output); err != nil {
		return nil, err
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if _, err := s.Resolve(nil); err != nil {
		return nil, err
	}
	return &Schema{root: m}, nil
}

// Validate implements precisely the closed profile accepted by CompileSchema.
// jsonschema-go v0.4.3 treats json.Number as a string in its type check. Our
// validator never converts numeric instances through binary64, even temporarily.
func (s *Schema) Validate(v any) error { return validateNode(s.root, v) }

func validateNode(s map[string]any, v any) error {
	switch s["type"] {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("expected object")
		}
		props := s["properties"].(map[string]any)
		if required, ok := s["required"].([]any); ok {
			for _, name := range required {
				if _, exists := m[name.(string)]; !exists {
					return errors.New("required property missing")
				}
			}
		}
		for name, value := range m {
			prop, exists := props[name]
			if !exists {
				return errors.New("unexpected property")
			}
			if err := validateNode(prop.(map[string]any), value); err != nil {
				return err
			}
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			return errors.New("expected array")
		}
		for _, value := range a {
			if err := validateNode(s["items"].(map[string]any), value); err != nil {
				return err
			}
		}
	case "number", "integer":
		n, ok := v.(json.Number)
		if !ok {
			return errors.New("expected JSON number token")
		}
		if s["type"] == "integer" && !integerToken(n.String()) {
			return errors.New("expected integer")
		}
	case "string":
		if _, ok := v.(string); !ok {
			return errors.New("expected string")
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return errors.New("expected boolean")
		}
	case "null":
		if v != nil {
			return errors.New("expected null")
		}
	}
	return nil
}

func integerToken(token string) bool {
	parts := strings.FieldsFunc(token, func(r rune) bool { return r == 'e' || r == 'E' })
	mantissa := strings.TrimPrefix(parts[0], "-")
	fraction := 0
	if dot := strings.IndexByte(mantissa, '.'); dot >= 0 {
		fraction = len(mantissa) - dot - 1
	}
	digits := strings.ReplaceAll(mantissa, ".", "")
	trimmed := strings.TrimRight(digits, "0")
	if trimmed == "" {
		return true
	}
	requiredExponent := int64(fraction - (len(digits) - len(trimmed)))
	if len(parts) == 1 {
		return requiredExponent <= 0
	}
	exponent, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return !strings.HasPrefix(parts[1], "-")
	}
	return exponent >= requiredExponent
}

func schemaNode(m map[string]any, output bool) error {
	allowed := map[string]bool{"type": true, "title": true, "description": true, "properties": true, "required": true, "additionalProperties": true, "items": true}
	for key := range m {
		if !allowed[key] {
			return fmt.Errorf("unsupported schema keyword %q", key)
		}
	}
	t, ok := m["type"].(string)
	if !ok {
		return errors.New("each schema node needs one explicit type")
	}
	switch t {
	case "object":
		if _, exists := m["items"]; exists {
			return errors.New("items belongs to array schemas")
		}
		props, ok := m["properties"].(map[string]any)
		if !ok {
			return errors.New("object schemas require properties")
		}
		if m["additionalProperties"] != false {
			return errors.New("object schemas require additionalProperties:false")
		}
		required := map[string]bool{}
		if r, exists := m["required"]; exists {
			a, ok := r.([]any)
			if !ok {
				return errors.New("required must be an array")
			}
			for _, v := range a {
				key, ok := v.(string)
				if !ok || required[key] {
					return errors.New("invalid or duplicate required property")
				}
				if _, exists := props[key]; !exists {
					return errors.New("required property has no schema")
				}
				required[key] = true
			}
		}
		for key, prop := range props {
			node, ok := prop.(map[string]any)
			if !ok {
				return errors.New("property schema must be an object")
			}
			if output && !required[key] {
				return errors.New("structured output requires every property to be required")
			}
			if err := schemaNode(node, output); err != nil {
				return fmt.Errorf("property %q: %w", key, err)
			}
		}
	case "array":
		if hasObjectKeywords(m) {
			return errors.New("object keywords on array schema")
		}
		node, ok := m["items"].(map[string]any)
		if !ok {
			return errors.New("arrays require an items schema")
		}
		return schemaNode(node, output)
	case "string", "number", "integer", "boolean", "null":
		if hasObjectKeywords(m) || m["items"] != nil {
			return errors.New("container keywords on scalar schema")
		}
	default:
		return fmt.Errorf("unsupported schema type %q", t)
	}
	return nil
}

func hasObjectKeywords(m map[string]any) bool {
	for _, k := range []string{"properties", "required", "additionalProperties"} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}
