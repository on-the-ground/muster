package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This opt-in test makes two actual model calls through the complete MCP path.
// It must not be reported as passed when only the deterministic fixtures ran.
func TestNativeWorker(t *testing.T) {
	manifest := os.Getenv("MUSTER_NATIVE_MANIFEST")
	if manifest == "" {
		t.Skip("set MUSTER_NATIVE_MANIFEST to opt in to a real model request")
	}
	if !filepath.IsAbs(manifest) {
		t.Fatal("MUSTER_NATIVE_MANIFEST must be absolute")
	}
	evidence := os.Getenv("MUSTER_NATIVE_EVIDENCE")
	if evidence == "" {
		evidence = filepath.Join(t.TempDir(), "native.jsonl")
	}
	binary := filepath.Join(t.TempDir(), "muster")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/muster").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "muster-native-smoke", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: exec.Command(binary, "-manifest", manifest, "-evidence", evidence)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	list, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil || len(list.Tools) != 1 || list.Tools[0].Name != "research-brief" {
		t.Fatalf("catalog: %v", err)
	}
	var previous string
	for range 2 {
		r, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "research-brief", Arguments: map[string]string{"question": "How does muster handle cancellation and retries? Cite only the registered sources and state what cannot be guaranteed."}})
		if err != nil {
			t.Fatal(err)
		}
		if r.IsError {
			for _, c := range r.Content {
				if text, ok := c.(*mcp.TextContent); ok {
					t.Log("tool failure:", text.Text)
				}
			}
			t.Fatal("native worker failed; no real-worker success claim")
		}
		if len(r.Content) != 1 {
			t.Fatal("missing structured result")
		}
		text, ok := r.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatal("missing exact JSON text")
		}
		var result struct {
			Summary       string                           `json:"summary"`
			Evidence      []struct{ Claim, Source string } `json:"evidence"`
			Uncertainties []string                         `json:"uncertainties"`
		}
		if err := json.Unmarshal([]byte(text.Text), &result); err != nil || result.Summary == "" || len(result.Evidence) == 0 {
			t.Fatal("missing research result")
		}
		for _, e := range result.Evidence {
			if e.Source != "muster-scope-notes" {
				t.Fatal("unregistered source citation")
			}
		}
		meta, ok := r.Meta["muster/evidence"].(map[string]any)
		if !ok || meta["sessionId"] == "" || meta["workerTerminated"] != true {
			t.Fatal("missing real session correlation or termination")
		}
		if p := os.Getenv("MUSTER_NATIVE_RESULT"); p != "" {
			if err := os.WriteFile(p, []byte(text.Text+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		current, ok := meta["sessionId"].(string)
		if !ok || current == "" || current == previous {
			t.Fatal("native worker reused or omitted session identity")
		}
		for _, name := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID", "CODEX_THREAD_ID"} {
			if parent := os.Getenv(name); parent != "" && parent == current {
				t.Fatal("worker reported a parent session identity")
			}
		}
		previous = current
	}
	t.Log("real CLI worker: registered research-brief -> tools/list -> tools/call -> two distinct native sessions -> schema-valid cited result; evidence:", evidence)
}
