package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/on-the-ground/muster/internal/contract"
)

// webManifest registers the fixture as a web-read tool behind a one-slot,
// one-place queue that records subsea/callRef.
func webManifest(t *testing.T, worker string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path, evidence := filepath.Join(dir, "manifest.json"), filepath.Join(dir, "evidence.jsonl")
	text := "Research with the web tools and return arguments unchanged."
	m := contract.Manifest{Version: 1, Pool: &contract.Pool{Workers: 1, QueueLimit: 1}, Correlation: &contract.Correlation{MetaKeys: []string{"subsea/callRef"}},
		Backends: map[string]contract.Backend{"worker": {Kind: "claude", Executable: worker}},
		Tools: []contract.Tool{{Name: "research", Description: "Research.", Version: "1", InputSchema: json.RawMessage(fixtureSchema), OutputSchema: json.RawMessage(fixtureSchema),
			Instructions: contract.Instructions{Version: "1", Text: text, SHA256: contract.Hash([]byte(text))},
			Binding:      contract.Binding{Backend: "worker", Extraction: "claude-structured-output", Context: []contract.ContextFile{}},
			Execution:    contract.Execution{Permissions: contract.PermissionWebRead, Effect: contract.EffectExternal, Cancellation: "terminate-process-group", Resources: contract.ResourcesWeb, TimeoutSeconds: 60}}}}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path, evidence
}

type record struct {
	Event       string            `json:"event"`
	Status      string            `json:"status"`
	Effect      string            `json:"effect"`
	Correlation map[string]string `json:"correlation"`
	SessionID   string            `json:"sessionId"`
	Effects     []struct {
		Tool, Target, Status string
	} `json:"effects"`
}

func records(t *testing.T, path string) []record {
	t.Helper()
	b, _ := os.ReadFile(path)
	var out []record
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var r record
		if json.Unmarshal(line, &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// find returns the records carrying a callRef, in journal order.
func find(rs []record, ref string) []record {
	var out []record
	for _, r := range rs {
		if r.Correlation["subsea/callRef"] == ref {
			out = append(out, r)
		}
	}
	return out
}

func waitRecord(t *testing.T, path, ref, event, status string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, r := range find(records(t, path), ref) {
			if r.Event == event && (status == "" || r.Status == status) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s/%s for %s: %+v", event, status, ref, records(t, path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Over stdio with the official client: the catalog declares the web effect,
// a busy pool queues and then serves, a full queue refuses, cancellation of a
// running call keeps its effect, and every record carries the caller's callRef.
func TestPoolCorrelationAndWebReadOverMCP(t *testing.T) {
	server, worker := binaries(t)
	path, evidence := webManifest(t, worker)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "pool-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: exec.Command(server, "-manifest", path, "-evidence", evidence)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	list, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil || len(list.Tools) != 1 {
		t.Fatalf("list: %v", err)
	}
	tool := list.Tools[0]
	if a := tool.Annotations; a == nil || !a.ReadOnlyHint || a.OpenWorldHint == nil || !*a.OpenWorldHint || a.DestructiveHint == nil || *a.DestructiveHint || a.IdempotentHint {
		t.Fatalf("annotations %+v", tool.Annotations)
	}
	if !strings.Contains(tool.Description, "queries and URLs are recorded") {
		t.Fatalf("description does not disclose the web effect: %s", tool.Description)
	}
	callAs := func(ctx context.Context, ref, mode string) chan *mcp.CallToolResult {
		out := make(chan *mcp.CallToolResult, 1)
		go func() {
			r, err := session.CallTool(ctx, &mcp.CallToolParams{Meta: mcp.Meta{"subsea/callRef": ref}, Name: "research", Arguments: json.RawMessage(`{"n":1,"mode":"` + mode + `"}`)})
			if err != nil {
				r = nil
			}
			out <- r
		}()
		return out
	}
	slowCtx, stopSlow := context.WithCancel(ctx)
	slow := callAs(slowCtx, "call#A", "web-sleep")
	waitRecord(t, evidence, "call#A", "job-dispatched", "")
	queued := callAs(ctx, "call#B", "web")
	waitRecord(t, evidence, "call#B", "job-admitted", "queued")
	if r := <-callAs(ctx, "call#C", "web"); r == nil || !r.IsError {
		t.Fatalf("full queue served a call: %+v", r)
	}
	stopSlow()
	<-slow
	r := <-queued
	if r == nil || r.IsError {
		t.Fatalf("queued call failed: %+v", r)
	}
	all := records(t, evidence)
	a := find(all, "call#A")
	if last := a[len(a)-1]; last.Status != "cancelled" || len(last.Effects) != 1 || last.Effects[0].Status != "requested" || last.Effects[0].Target != "https://slow.example/" {
		t.Fatalf("cancelled running call lost its effect: %+v", a)
	}
	b := find(all, "call#B")
	if len(b) != 3 || b[0].Status != "queued" || b[1].Event != "job-dispatched" || b[2].Status != "succeeded" || len(b[2].Effects) != 2 || b[2].SessionID == "" {
		t.Fatalf("queued call records %+v", b)
	}
	c := find(all, "call#C")
	if len(c) != 1 || c[0].Status != "overloaded" || c[0].Effect != "not-started" {
		t.Fatalf("overload record %+v", c)
	}
}

// A queued call cancelled over MCP ends with no worker started.
func TestQueuedCallCancelledOverMCPNeverStarts(t *testing.T) {
	server, worker := binaries(t)
	path, evidence := webManifest(t, worker)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "pool-client", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: exec.Command(server, "-manifest", path, "-evidence", evidence)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(ctx context.Context, ref, mode string) chan error {
		out := make(chan error, 1)
		go func() {
			_, err := session.CallTool(ctx, &mcp.CallToolParams{Meta: mcp.Meta{"subsea/callRef": ref}, Name: "research", Arguments: json.RawMessage(`{"n":1,"mode":"` + mode + `"}`)})
			out <- err
		}()
		return out
	}
	runCtx, stopRun := context.WithCancel(ctx)
	running := call(runCtx, "call#run", "web-sleep")
	waitRecord(t, evidence, "call#run", "job-dispatched", "")
	waitCtx, stopWait := context.WithCancel(ctx)
	waiting := call(waitCtx, "call#wait", "web")
	waitRecord(t, evidence, "call#wait", "job-admitted", "queued")
	stopWait()
	<-waiting
	waitRecord(t, evidence, "call#wait", "job-finished", "cancelled_before_dispatch")
	for _, r := range find(records(t, evidence), "call#wait") {
		if r.Event == "job-dispatched" || r.SessionID != "" || len(r.Effects) != 0 {
			t.Fatalf("cancelled queued call ran: %+v", r)
		}
	}
	stopRun()
	<-running
}
