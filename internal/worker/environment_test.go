package worker

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/on-the-ground/muster/internal/contract"
)

func TestWorkerEnvironmentDoesNotInheritParentSessionOrOtherSecrets(t *testing.T) {
	for _, name := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID", "CODEX_THREAD_ID", "MUSTER_UNRELATED_SECRET"} {
		t.Setenv(name, "parent-only-marker")
	}
	t.Setenv("MUSTER_OPERATOR_BINDING", "operator-selected")
	t.Setenv("CLAUDE_CODE_DISABLE_CLAUDE_MDS", "0")
	for _, kind := range []string{"claude", "codex"} {
		env := workerEnvironment(kind, []string{"MUSTER_OPERATOR_BINDING"})
		for _, name := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID", "CODEX_THREAD_ID", "MUSTER_UNRELATED_SECRET"} {
			if envValue(env, name) != "" {
				t.Fatalf("inherited disallowed name %s", name)
			}
		}
		if envValue(env, "MUSTER_OPERATOR_BINDING") != "operator-selected" {
			t.Fatal("explicit operator binding missing")
		}
		if kind == "claude" && (envValue(env, "CLAUDE_CODE_DISABLE_CLAUDE_MDS") != "1" || envValue(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY") != "1") {
			t.Fatal("memory controls can be weakened by the parent")
		}
	}
	binary := fixture(t)
	for _, kind := range []string{"claude", "codex"} {
		b, err := New(contract.Backend{Kind: kind, Executable: binary})
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		identity := b.Identity()
		if identity.Kind != kind || identity.Path != binary || identity.Version != "fixture-cli 1.0.0" {
			t.Fatal("missing backend identity")
		}
		p, err := b.Start(context.Background(), Request{Prompt: []byte(`{"instructions":{"text":"Echo"},"arguments":{"mode":"permission"}}`), Schema: json.RawMessage(`{"type":"object"}`)})
		if err != nil {
			t.Fatal(err)
		}
		if out, err := p.Wait(); err != nil || !out.Terminated {
			t.Fatal("child observed inherited state or missing isolation flags")
		}
	}
	if _, err := New(contract.Backend{Kind: "claude", Executable: binary, EnvironmentAllowlist: []string{"CLAUDE_CODE_SESSION_ID"}}); err == nil {
		t.Fatal("operator cannot enable parent-session inheritance")
	}
}

func TestCapabilityProbeRequiresDeclaredFlag(t *testing.T) {
	for _, tc := range []struct {
		help string
		want bool
	}{
		{"  --output-schema <FILE>\n", true}, {"  --output-schema-file <FILE>\n", false},
		{"  -o, --output-schema <FILE>\n", true}, {"Description mentions --output-schema support", false},
	} {
		if got := hasFlag(tc.help, "--output-schema"); got != tc.want {
			t.Fatalf("incorrect flag match: %q", tc.help)
		}
	}
}

func TestClaudeSchemaArgumentRejectedBeforeLaunch(t *testing.T) {
	binary := fixture(t)
	b, _ := New(contract.Backend{Kind: "claude", Executable: binary})
	if _, err := b.Start(context.Background(), Request{Schema: make([]byte, contract.MaxClaudeSchemaBytes+1)}); err == nil {
		t.Fatal("oversized schema passed to the OS")
	}
	if !filepath.IsAbs(b.Identity().Path) {
		t.Fatal("relative backend identity")
	}
}
