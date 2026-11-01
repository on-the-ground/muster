package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// nativeWebEvidence is the part of a job-finished record these tests check.
type nativeWebEvidence struct {
	Status      string   `json:"status"`
	SessionID   string   `json:"sessionId"`
	DeniedTools []string `json:"deniedTools"`
	Effects     []struct {
		Tool         string   `json:"tool"`
		Target       string   `json:"target"`
		Status       string   `json:"status"`
		ObservedURLs []string `json:"observedUrls"`
	} `json:"effects"`
}

func nativeWebSession(t *testing.T, manifest string) (*mcp.ClientSession, context.Context, string) {
	t.Helper()
	if !filepath.IsAbs(manifest) {
		t.Fatal("manifest path must be absolute")
	}
	evidence := os.Getenv("MUSTER_NATIVE_EVIDENCE")
	if evidence == "" {
		evidence = filepath.Join(t.TempDir(), "native-web.jsonl")
	}
	binary := filepath.Join(t.TempDir(), "muster")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/muster").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "muster-native-web", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: exec.Command(binary, "-manifest", manifest, "-evidence", evidence)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session, ctx, evidence
}

func resultEvidence(t *testing.T, r *mcp.CallToolResult) nativeWebEvidence {
	t.Helper()
	b, err := json.Marshal(r.Meta["muster/evidence"])
	if err != nil {
		t.Fatal(err)
	}
	var e nativeWebEvidence
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

// Opt-in real web research: one model request through the full MCP path.
// It checks that native tool uses are recorded and that every cited URL was
// observed in a search result or fetched. Network policy may block fetches;
// those must then appear as error effects, not as cited evidence.
func TestNativeWebWorker(t *testing.T) {
	manifest := os.Getenv("MUSTER_NATIVE_WEB_MANIFEST")
	if manifest == "" {
		t.Skip("set MUSTER_NATIVE_WEB_MANIFEST to opt in to a real web-research request")
	}
	session, ctx, evidence := nativeWebSession(t, manifest)
	question := os.Getenv("MUSTER_NATIVE_WEB_QUESTION")
	if question == "" {
		question = "Which direct red (광역) bus routes run between Suji-gu, Yongin and Gangnam Station? Name the routes and where each boards in Suji-gu."
	}
	r, err := session.CallTool(ctx, &mcp.CallToolParams{Meta: mcp.Meta{"subsea/callRef": "native-web#1"}, Name: "web-research", Arguments: map[string]string{"question": question}})
	if err != nil {
		t.Fatal(err)
	}
	e := resultEvidence(t, r)
	if r.IsError {
		t.Fatalf("native web worker failed (%s); no real-worker success claim. evidence: %s", e.Status, evidence)
	}
	var result struct {
		Summary  string `json:"summary"`
		Evidence []struct {
			Claim, URL, ObservedAt string
		} `json:"evidence"`
		Uncertainties []string `json:"uncertainties"`
	}
	text := r.Content[0].(*mcp.TextContent).Text
	if err := json.Unmarshal([]byte(text), &result); err != nil || result.Summary == "" {
		t.Fatalf("result: %v %s", err, text)
	}
	seen := map[string]bool{}
	searches := 0
	for _, eff := range e.Effects {
		if eff.Tool == "WebSearch" && eff.Status == "ok" {
			searches++
		}
		for _, u := range eff.ObservedURLs {
			seen[u] = true
		}
		if eff.Tool == "WebFetch" && eff.Status == "ok" {
			seen[eff.Target] = true
		}
	}
	if searches == 0 {
		t.Fatalf("no successful WebSearch effect recorded: %+v", e.Effects)
	}
	for _, c := range result.Evidence {
		if !seen[c.URL] {
			t.Errorf("cited URL was not observed in any recorded tool result: %s", c.URL)
		}
	}
	for _, name := range []string{"CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_REMOTE_SESSION_ID"} {
		if parent := os.Getenv(name); parent != "" && parent == e.SessionID {
			t.Fatal("worker reported a parent session identity")
		}
	}
	if p := os.Getenv("MUSTER_NATIVE_RESULT"); p != "" {
		_ = os.WriteFile(p, []byte(text+"\n"), 0600)
	}
	t.Logf("real web worker: %d effects, %d cited URLs, session %s, evidence %s", len(e.Effects), len(result.Evidence), e.SessionID, evidence)
}

// Opt-in native permission check: with WebFetch restricted to one domain, a
// fetch elsewhere must be refused by the CLI and recorded as denied.
func TestNativeWebFetchDomainDenial(t *testing.T) {
	manifest := os.Getenv("MUSTER_NATIVE_WEB_DENY_MANIFEST")
	if manifest == "" {
		t.Skip("set MUSTER_NATIVE_WEB_DENY_MANIFEST (webFetchDomains not covering example.com) to opt in")
	}
	session, ctx, evidence := nativeWebSession(t, manifest)
	r, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "web-research", Arguments: map[string]string{"question": "Call WebFetch exactly once on https://example.com/ and report the page title. If the fetch is refused, say so under uncertainties."}})
	if err != nil {
		t.Fatal(err)
	}
	e := resultEvidence(t, r)
	denied := false
	for _, eff := range e.Effects {
		if eff.Tool == "WebFetch" && strings.Contains(eff.Target, "example.com") && eff.Status == "ok" {
			t.Fatalf("WebFetch outside the allowed domains succeeded: %+v", e.Effects)
		}
		denied = denied || (eff.Tool == "WebFetch" && eff.Status == "denied")
	}
	if !denied || len(e.DeniedTools) == 0 {
		t.Fatalf("no native denial recorded (status %s): %+v %v; evidence %s", e.Status, e.Effects, e.DeniedTools, evidence)
	}
	t.Logf("native denial recorded: %+v", e.DeniedTools)
}
