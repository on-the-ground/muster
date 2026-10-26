package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/on-the-ground/muster/internal/contract"
	"github.com/on-the-ground/muster/internal/strictjson"
)

const fixtureSchema = `{"type":"object","properties":{"n":{"type":"number"},"mode":{"type":"string"}},"required":["n","mode"],"additionalProperties":false}`

func binaries(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	server := filepath.Join(dir, "muster")
	worker := filepath.Join(dir, "worker")
	for _, target := range []struct{ path, pkg string }{{server, "../../cmd/muster"}, {worker, "../testworker"}} {
		cmd := exec.Command("go", "build", "-o", target.path, target.pkg)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
	}
	return server, worker
}

func manifest(t *testing.T, worker, kind string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	evidence := filepath.Join(dir, "evidence.jsonl")
	permissions, extraction, resources := "no-tools", "claude-structured-output", "captured-context"
	if kind == "codex" {
		permissions, extraction, resources = "read-only-sandbox", "codex-output-file", "live-workspace"
	}
	text := "Return arguments unchanged as one JSON object."
	m := contract.Manifest{Version: 1, Backends: map[string]contract.Backend{"worker": {Kind: kind, Executable: worker}}, Tools: []contract.Tool{{Name: "echo", Description: "Echo exact JSON numbers.", Version: "1", InputSchema: json.RawMessage(fixtureSchema), OutputSchema: json.RawMessage(fixtureSchema), Instructions: contract.Instructions{Version: "1", Text: text, SHA256: contract.Hash([]byte(text))}, Binding: contract.Binding{Backend: "worker", Extraction: extraction, Context: []contract.ContextFile{}}, Execution: contract.Execution{Permissions: permissions, Effect: "read-only", Cancellation: "terminate-process-group", Resources: resources, TimeoutSeconds: 1}}}}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path, evidence
}

type rawClient struct {
	in    io.WriteCloser
	lines chan []byte
	errs  chan error
	cmd   *exec.Cmd
}

func start(t *testing.T, binary, path, evidence string) *rawClient {
	t.Helper()
	cmd := exec.Command(binary, "-manifest", path, "-evidence", evidence)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &rawClient{in: in, lines: make(chan []byte, 10), errs: make(chan error, 1), cmd: cmd}
	go func() {
		s := bufio.NewScanner(out)
		s.Buffer(make([]byte, 4096), 4<<20)
		for s.Scan() {
			c.lines <- append([]byte{}, s.Bytes()...)
		}
		c.errs <- s.Err()
	}()
	t.Cleanup(func() {
		_ = in.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	c.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"conformance","version":"1"}}}`)
	init := c.receive(t)
	var result struct {
		Result struct {
			Capabilities struct {
				Tools struct {
					ListChanged bool `json:"listChanged"`
				} `json:"tools"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(init, &result); err != nil || bytes.Contains(init, []byte(`"error"`)) || result.Result.Capabilities.Tools.ListChanged {
		t.Fatalf("bad initialization: %s", init)
	}
	c.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return c
}
func (c *rawClient) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		t.Fatal(err)
	}
}
func (c *rawClient) receive(t *testing.T) []byte {
	t.Helper()
	select {
	case b := <-c.lines:
		return b
	case err := <-c.errs:
		t.Fatalf("server closed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("MCP response timeout")
	}
	return nil
}

func TestStdioConformanceBothBackends(t *testing.T) {
	server, worker := binaries(t)
	for _, kind := range []string{"claude", "codex"} {
		t.Run(kind, func(t *testing.T) {
			path, evidence := manifest(t, worker, kind)
			c := start(t, server, path, evidence)
			c.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
			list := c.receive(t)
			if !bytes.Contains(list, []byte(`"name":"echo"`)) || !bytes.Contains(list, []byte(`"outputSchema"`)) {
				t.Fatalf("catalog missing: %s", list)
			}
			for i, tc := range []struct{ args, want string }{
				{`{"n":9007199254740993,"mode":"permission"}`, "succeeded"},
				{`{"n":0.12345678901234567890123456789,"mode":"ok"}`, "succeeded"},
				{`{"n":"bad","mode":"ok"}`, "invalid_input"},
				{`{"n":1,"mode":"invalid"}`, "output_contract_violation"},
				{`{"n":1,"mode":"duplicate"}`, "output_contract_violation"},
				{`{"n":1,"mode":"crash"}`, "execution_failed"},
				{`{"n":1,"mode":"sleep"}`, "timeout"},
			} {
				request := struct {
					JSONRPC string `json:"jsonrpc"`
					ID      int    `json:"id"`
					Method  string `json:"method"`
					Params  struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"params"`
				}{JSONRPC: "2.0", ID: i + 10, Method: "tools/call"}
				request.Params.Name = "echo"
				request.Params.Arguments = json.RawMessage(tc.args)
				b, _ := json.Marshal(request)
				c.send(t, string(b))
				response := c.receive(t)
				if _, err := strictjson.Object(response); err != nil {
					t.Fatal(err)
				}
				var r struct {
					Result struct {
						IsError           bool                     `json:"isError"`
						StructuredContent json.RawMessage          `json:"structuredContent"`
						Meta              map[string]proxyEvidence `json:"_meta"`
					} `json:"result"`
				}
				if err := json.Unmarshal(response, &r); err != nil {
					t.Fatal(err)
				}
				e := r.Result.Meta["muster/evidence"]
				if e.Status != tc.want || r.Result.IsError != (tc.want != "succeeded") {
					t.Fatalf("%s: %s", tc.want, response)
				}
				if tc.want == "succeeded" && string(r.Result.StructuredContent) != tc.args {
					t.Fatalf("number tokens changed: %s", response)
				}
				if tc.want != "succeeded" && len(r.Result.StructuredContent) > 0 {
					t.Fatal("failure concealed as a Value")
				}
				if bytes.Contains(response, []byte("PRIVATE_CREDENTIAL")) {
					t.Fatal("worker stderr leaked")
				}
			}
			b, err := os.ReadFile(evidence)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, []byte("PRIVATE_CREDENTIAL")) || bytes.Contains(b, []byte("9007199254740993")) {
				t.Fatal("raw data leaked to evidence")
			}
		})
	}
}

type proxyEvidence struct {
	Status           string `json:"status"`
	WorkerTerminated bool   `json:"workerTerminated"`
	CancelRequested  bool   `json:"cancelRequested"`
}

func TestMCPCancellationStopsWorker(t *testing.T) {
	server, worker := binaries(t)
	path, evidence := manifest(t, worker, "claude")
	c := start(t, server, path, evidence)
	c.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"n":1,"mode":"sleep"}}}`)
	// Wait for dispatch admission before cancelling the MCP request.
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, _ := os.ReadFile(evidence)
		if bytes.Contains(b, []byte("worker-started")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker not admitted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3,"reason":"test cancellation"}}`)
	for {
		b, _ := os.ReadFile(evidence)
		for _, line := range bytes.Split(b, []byte("\n")) {
			var e proxyEvidence
			_ = json.Unmarshal(line, &e)
			if e.Status == "cancelled" && e.WorkerTerminated && e.CancelRequested {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancellation not recorded: %s", b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDisconnectStopsWorker(t *testing.T) {
	server, worker := binaries(t)
	path, evidence := manifest(t, worker, "codex")
	c := start(t, server, path, evidence)
	c.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"n":1,"mode":"sleep"}}}`)
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, _ := os.ReadFile(evidence)
		if bytes.Contains(b, []byte("worker-started")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker was not started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.in.Close(); err != nil {
		t.Fatal(err)
	}
	for {
		b, _ := os.ReadFile(evidence)
		for _, line := range bytes.Split(b, []byte("\n")) {
			var e proxyEvidence
			_ = json.Unmarshal(line, &e)
			if e.Status == "cancelled" && e.WorkerTerminated && e.CancelRequested {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("disconnect did not stop worker: %s", b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGuardRejectsDuplicateKeysBeforeSDK(t *testing.T) {
	for _, line := range []string{`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"echo","name":"evil"}}`, `{"a":{"n":1,"n":2}}`} {
		r := NewGuardedReader(io.NopCloser(strings.NewReader(line + "\n")))
		if _, err := io.ReadAll(r); err == nil {
			t.Fatal("duplicate keys accepted")
		}
	}
	raw := `{"n":1.2300e+40}` + "\n"
	r := NewGuardedReader(io.NopCloser(strings.NewReader(raw)))
	got, err := io.ReadAll(r)
	if err != nil || string(got) != raw {
		t.Fatal("wire changed")
	}
}

// Use an unmodified official SDK client as well as the raw token checks above.
func TestOfficialSDKClient(t *testing.T) {
	server, worker := binaries(t)
	path, evidence := manifest(t, worker, "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "generic-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: exec.Command(server, "-manifest", path, "-evidence", evidence)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	list, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil || len(list.Tools) != 1 {
		t.Fatalf("list: %v", err)
	}
	r, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: json.RawMessage(`{"n":9007199254740993,"mode":"ok"}`)})
	if err != nil || r.IsError {
		t.Fatalf("call: %v %+v", err, r)
	}
	if len(r.Content) != 1 || !strings.Contains(r.Content[0].(*mcp.TextContent).Text, "9007199254740993") {
		t.Fatal("exact text result missing")
	}
}
