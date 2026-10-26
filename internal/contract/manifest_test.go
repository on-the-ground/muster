package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-the-ground/muster/internal/strictjson"
)

const testSchema = `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}`

func validManifest() Manifest {
	return Manifest{Version: 1, Backends: map[string]Backend{"worker": {Kind: "claude", Executable: "claude"}}, Tools: []Tool{{Name: "inspect", Description: "Inspect captured facts.", Version: "1", InputSchema: json.RawMessage(testSchema), OutputSchema: json.RawMessage(testSchema), Instructions: Instructions{Version: "1", Text: "Return n unchanged.", SHA256: Hash([]byte("Return n unchanged."))}, Binding: Binding{Backend: "worker", Extraction: "claude-structured-output", Context: []ContextFile{}}, Execution: Execution{Permissions: "no-tools", Effect: "read-only", Cancellation: "terminate-process-group", Resources: "captured-context", TimeoutSeconds: 10}}}}
}

func loadManifest(t *testing.T, m Manifest) (*Catalog, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "manifest.json")
	b, _ := json.Marshal(m)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestManifestRejectsUnenforceableOrAmbiguousContracts(t *testing.T) {
	cases := map[string]func(*Manifest){
		"hash":        func(m *Manifest) { m.Tools[0].Instructions.Text = "changed" },
		"reuse":       func(m *Manifest) { m.Tools[0].Execution.ReuseSession = true },
		"permissions": func(m *Manifest) { m.Tools[0].Execution.Permissions = "read-anything" },
		"effect":      func(m *Manifest) { m.Tools[0].Execution.Effect = "effectful" },
		"extraction":  func(m *Manifest) { m.Tools[0].Binding.Extraction = "text-json" },
		"binding":     func(m *Manifest) { m.Tools[0].Binding.Backend = "unknown" },
		"duplicate":   func(m *Manifest) { m.Tools = append(m.Tools, m.Tools[0]) },
		"schema": func(m *Manifest) {
			m.Tools[0].OutputSchema = json.RawMessage(`{"type":"object","properties":{},"minimum":9007199254740993,"additionalProperties":false}`)
		},
		"optional output": func(m *Manifest) {
			m.Tools[0].OutputSchema = json.RawMessage(`{"type":"object","properties":{"n":{"type":"number"}},"additionalProperties":false}`)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			change(&m)
			if _, err := loadManifest(t, m); err == nil {
				t.Fatal("accepted invalid contract")
			}
		})
	}
}

func TestSchemaValidatesLargeIntegerWithoutRounding(t *testing.T) {
	s, err := CompileSchema(json.RawMessage(testSchema), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"n":9007199254740993}`, `{"n":1.234567890123456789e40}`} {
		v, err := strictjson.Decode([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Validate(v); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{`{"n":9007199254740993.1}`, `{"n":"1"}`, `{"n":1,"extra":true}`} {
		v, _ := strictjson.Decode([]byte(raw))
		if s.Validate(v) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestContextCapturedOnce(t *testing.T) {
	dir := t.TempDir()
	contextPath := filepath.Join(dir, "facts.txt")
	if err := os.WriteFile(contextPath, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	m := validManifest()
	m.Tools[0].Binding.Context = []ContextFile{{Name: "facts", Path: contextPath}}
	c, err := loadManifest(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contextPath, []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	if c.Tools[0].Context[0].Content != "before" {
		t.Fatal("context not pinned")
	}
	if !strings.EqualFold(c.Tools[0].Context[0].SHA256, Hash([]byte("before"))) {
		t.Fatal("incorrect evidence hash")
	}
}

func TestExactIntegerExponentClassification(t *testing.T) {
	for _, tc := range []struct {
		token   string
		integer bool
	}{
		{"0e-999999999999999999999999", true}, {"1e9999999999999999999999", true},
		{"1e-9999999999999999999999", false}, {"100.00e-2", true}, {"100.00e-3", false},
		{"-0.000", true}, {"9007199254740993.000", true}, {"9007199254740993.00000000000001", false},
	} {
		if integerToken(tc.token) != tc.integer {
			t.Fatalf("wrong classification for %s", tc.token)
		}
	}
}
