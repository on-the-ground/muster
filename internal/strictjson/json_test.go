package strictjson

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNumbersRemainTokens(t *testing.T) {
	raw := `{"large":9007199254740993,"decimal":0.12345678901234567890123456789,"exponent":1.2300e+40}`
	m, err := Object([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"large": "9007199254740993", "decimal": "0.12345678901234567890123456789", "exponent": "1.2300e+40"} {
		if got := m[key].(json.Number).String(); got != want {
			t.Fatalf("%s rounded: %s", key, got)
		}
	}
}

func TestRejectAmbiguousAndMalformedJSON(t *testing.T) {
	for _, raw := range []string{`{"a":1,"\u0061":2}`, `{"x":{"a":1,"a":2}}`, `[1,]`, `{} {}`, `{"x":NaN}`, `{"x":1]`, ``, strings.Repeat("[", 130) + strings.Repeat("]", 130)} {
		t.Run(raw[:min(len(raw), 30)], func(t *testing.T) {
			if _, err := Decode([]byte(raw)); err == nil {
				t.Fatalf("accepted %q", raw)
			}
		})
	}
}

func TestConfigurationTagsAreExactButGeneralKeysAreCaseSensitive(t *testing.T) {
	var target struct {
		Version int `json:"version"`
	}
	for _, raw := range []string{`{"VERSION":1}`, `{"version":1,"Version":2}`} {
		if err := Unmarshal([]byte(raw), &target); err == nil {
			t.Fatal("inexact struct field accepted")
		}
	}
	m, err := Object([]byte(`{"a":1,"A":2}`))
	if err != nil || len(m) != 2 {
		t.Fatal("ordinary JSON keys changed semantics")
	}
	fields, err := Fields([]byte(`{"structured_output": {"n": 1.2300e+40},"is_error":false}`))
	if err != nil || string(fields["structured_output"]) != `{"n": 1.2300e+40}` {
		t.Fatal("raw output extraction changed number spelling")
	}
	if _, err := Fields([]byte(`{"structured_output":{"n":1,"n":2}}`)); err == nil {
		t.Fatal("nested duplicate accepted by envelope extraction")
	}
}
