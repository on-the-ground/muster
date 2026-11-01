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

func TestManifestRejectsCaseAliasesAndOversizedSchema(t *testing.T) {
	m := validManifest()
	b, _ := json.Marshal(m)
	for _, body := range []string{
		strings.Replace(string(b), `"version":1`, `"version":1,"VERSION":2`, 1),
		strings.Replace(string(b), `"executable":"claude"`, `"executable":"claude","Executable":"/tmp/untrusted"`, 1),
	} {
		p := filepath.Join(t.TempDir(), "manifest.json")
		_ = os.WriteFile(p, []byte(body), 0600)
		if _, err := Load(p); err == nil {
			t.Fatal("case-insensitive configuration alias accepted")
		}
	}
	m = validManifest()
	m.Tools[0].OutputSchema = json.RawMessage(strings.Replace(testSchema, `"type":"object"`, `"description":"`+strings.Repeat("x", MaxClaudeSchemaBytes)+`","type":"object"`, 1))
	if _, err := loadManifest(t, m); err == nil {
		t.Fatal("oversized Claude schema registered")
	}
	m = validManifest()
	m.Backends["worker"] = Backend{Kind: "claude", Executable: "claude", EnvironmentAllowlist: []string{"CLAUDE_CODE_SESSION_ID"}}
	if _, err := loadManifest(t, m); err == nil {
		t.Fatal("parent session binding registered")
	}
}

func TestBackendIdentityParticipatesInContractHash(t *testing.T) {
	m := validManifest()
	a, err := loadManifest(t, m)
	if err != nil {
		t.Fatal(err)
	}
	b := m.Backends["worker"]
	b.Executable = "/opt/other/claude"
	m.Backends["worker"] = b
	c, err := loadManifest(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if a.Tools[0].ContractHash == c.Tools[0].ContractHash {
		t.Fatal("backend executable change did not change contract hash")
	}
}

func TestCodexRequiresExplicitExperimentalHostReadScope(t *testing.T) {
	m := validManifest()
	m.Backends["worker"] = Backend{Kind: "codex", Executable: "codex"}
	m.Tools[0].Binding.Experimental = true
	m.Tools[0].Binding.Extraction = "codex-output-file"
	m.Tools[0].Execution.Permissions = "read-only-sandbox"
	m.Tools[0].Execution.Resources = "host-filesystem-readable"
	if _, err := loadManifest(t, m); err != nil {
		t.Fatal(err)
	}
	m.Tools[0].Binding.Experimental = false
	if _, err := loadManifest(t, m); err == nil {
		t.Fatal("non-experimental Codex contract accepted")
	}
	m.Tools[0].Binding.Experimental = true
	m.Tools[0].Execution.Resources = "live-workspace"
	if _, err := loadManifest(t, m); err == nil {
		t.Fatal("ambiguous Codex read scope accepted")
	}
}

func webManifest() Manifest {
	m := validManifest()
	m.Tools[0].Execution.Permissions = PermissionWebRead
	m.Tools[0].Execution.Effect = EffectExternal
	m.Tools[0].Execution.Resources = ResourcesWeb
	return m
}

func TestWebReadProfile(t *testing.T) {
	m := webManifest()
	m.Tools[0].Binding.WebFetchDomains = []string{"gbis.go.kr", "namu.wiki"}
	if _, err := loadManifest(t, m); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Manifest){
		"read-only effect":  func(m *Manifest) { m.Tools[0].Execution.Effect = EffectReadOnly },
		"captured resource": func(m *Manifest) { m.Tools[0].Execution.Resources = ResourcesCaptured },
		"no-tools external": func(m *Manifest) { m.Tools[0].Execution.Permissions = PermissionNoTools },
		"domain casing":     func(m *Manifest) { m.Tools[0].Binding.WebFetchDomains = []string{"Namu.Wiki"} },
		"domain path":       func(m *Manifest) { m.Tools[0].Binding.WebFetchDomains = []string{"namu.wiki/w"} },
		"domain wildcard":   func(m *Manifest) { m.Tools[0].Binding.WebFetchDomains = []string{"*.go.kr"} },
		"duplicate domain":  func(m *Manifest) { m.Tools[0].Binding.WebFetchDomains = []string{"a.example", "a.example"} },
		"codex web": func(m *Manifest) {
			m.Backends["worker"] = Backend{Kind: "codex", Executable: "codex"}
			m.Tools[0].Binding.Experimental = true
			m.Tools[0].Binding.Extraction = "codex-output-file"
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := webManifest()
			change(&m)
			if _, err := loadManifest(t, m); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	m = validManifest()
	m.Tools[0].Binding.WebFetchDomains = []string{"a.example"}
	if _, err := loadManifest(t, m); err == nil {
		t.Fatal("webFetchDomains accepted on a no-tools tool")
	}
}

func TestPoolAndCorrelationConfiguration(t *testing.T) {
	c, err := loadManifest(t, validManifest())
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool != (Pool{Workers: 1}) || c.Correlation != nil {
		t.Fatalf("defaults %+v %v", c.Pool, c.Correlation)
	}
	m := validManifest()
	m.Pool = &Pool{Workers: 4, QueueLimit: 16}
	m.Correlation = &Correlation{MetaKeys: []string{"subsea/callRef"}}
	if c, err = loadManifest(t, m); err != nil || c.Pool.Workers != 4 || c.Pool.QueueLimit != 16 || c.Correlation[0] != "subsea/callRef" {
		t.Fatalf("configured %+v %v", c, err)
	}
	for name, change := range map[string]func(*Manifest){
		"no workers":    func(m *Manifest) { m.Pool = &Pool{Workers: 0} },
		"too many":      func(m *Manifest) { m.Pool = &Pool{Workers: MaxWorkers + 1} },
		"negative":      func(m *Manifest) { m.Pool = &Pool{Workers: 1, QueueLimit: -1} },
		"queue":         func(m *Manifest) { m.Pool = &Pool{Workers: 1, QueueLimit: MaxQueueLimit + 1} },
		"no keys":       func(m *Manifest) { m.Correlation = &Correlation{} },
		"duplicate key": func(m *Manifest) { m.Correlation = &Correlation{MetaKeys: []string{"a", "a"}} },
		"bad key":       func(m *Manifest) { m.Correlation = &Correlation{MetaKeys: []string{"a b"}} },
	} {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			change(&m)
			if _, err := loadManifest(t, m); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	b, _ := json.Marshal(validManifest())
	body := strings.Replace(string(b), `"version":1`, `"version":1,"pool":{"workers":2,"queueLimit":1,"Workers":9}`, 1)
	p := filepath.Join(t.TempDir(), "manifest.json")
	_ = os.WriteFile(p, []byte(body), 0600)
	if _, err := Load(p); err == nil {
		t.Fatal("case alias in pool accepted")
	}
}
