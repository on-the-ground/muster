// Package strictjson decodes JSON without rounding numbers or accepting duplicate keys.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

const MaxBytes = 4 << 20

// Decode preserves number tokens, including their spelling, and checks every object.
func Decode(raw []byte) (any, error) {
	if len(raw) > MaxBytes || !utf8.Valid(raw) {
		return nil, errors.New("JSON exceeds size limit or is not UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := value(d, 0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("expected exactly one JSON value")
	}
	return v, nil
}

func value(d *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, errors.New("JSON nesting limit exceeded")
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, compound := t.(json.Delim)
	if !compound {
		return t, nil
	}
	switch delim {
	case '{':
		m := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			k, ok := key.(string)
			if !ok {
				return nil, errors.New("object key must be a string")
			}
			if _, exists := m[k]; exists {
				return nil, fmt.Errorf("duplicate object key %q", k)
			}
			v, err := value(d, depth+1)
			if err != nil {
				return nil, err
			}
			m[k] = v
		}
		if _, err := d.Token(); err != nil {
			return nil, err
		}
		return m, nil
	case '[':
		a := []any{}
		for d.More() {
			v, err := value(d, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		if _, err := d.Token(); err != nil {
			return nil, err
		}
		return a, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}

// Object checks the top-level MCP arguments/result shape as well as nested JSON.
func Object(raw []byte) (map[string]any, error) {
	v, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("expected a JSON object")
	}
	return m, nil
}

// Fields validates an envelope once while retaining each field's original JSON.
// It is used for CLI output extraction without reparsing a large envelope.
func Fields(raw []byte) (map[string]json.RawMessage, error) {
	if len(raw) > MaxBytes || !utf8.Valid(raw) {
		return nil, errors.New("invalid envelope size or encoding")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, errors.New("expected object envelope")
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := t.(string)
		if !ok {
			return nil, errors.New("invalid envelope key")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate envelope key")
		}
		start := d.InputOffset()
		if _, err := value(d, 1); err != nil {
			return nil, err
		}
		span := bytes.TrimSpace(raw[start:d.InputOffset()])
		if len(span) == 0 || span[0] != ':' {
			return nil, errors.New("invalid envelope separator")
		}
		fields[key] = bytes.TrimSpace(span[1:])
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing envelope data")
	}
	return fields, nil
}

func Unmarshal(raw []byte, target any) error {
	v, err := Decode(raw)
	if err != nil {
		return err
	}
	if err := exactFields(v, reflect.TypeOf(target)); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(target)
}

// Struct configuration keys must match JSON tags exactly. General JSON objects
// retain case-sensitive keys; only configuration struct decoding is restricted.
func exactFields(v any, t reflect.Type) error {
	if t == nil {
		return errors.New("nil decode target")
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeFor[json.RawMessage]() || v == nil {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.IsExported() {
				name := strings.Split(f.Tag.Get("json"), ",")[0]
				if name == "" {
					name = f.Name
				}
				if name != "-" {
					fields[name] = f.Type
				}
			}
		}
		for name, value := range m {
			field, ok := fields[name]
			if !ok {
				return fmt.Errorf("unknown or incorrectly cased configuration field %q", name)
			}
			if err := exactFields(value, field); err != nil {
				return err
			}
		}
	case reflect.Map:
		if m, ok := v.(map[string]any); ok {
			for _, value := range m {
				if err := exactFields(value, t.Elem()); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		if a, ok := v.([]any); ok {
			for _, value := range a {
				if err := exactFields(value, t.Elem()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
