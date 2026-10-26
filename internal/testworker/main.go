// testworker is an offline CLI protocol fixture, NOT an agent backend. It is
// built explicitly by conformance tests and never selected by the muster CLI.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--fixture-child" {
		for {
			time.Sleep(time.Hour)
		}
	}
	for _, a := range args {
		if a == "--help" {
			fmt.Println("--json-schema --output-format --tools --strict-mcp-config --mcp-config --permission-mode --setting-sources --no-session-persistence --disable-slash-commands --settings --bare --output-schema --output-last-message --sandbox --ephemeral --ignore-user-config --ignore-rules")
			return
		}
	}
	var input struct {
		Arguments    json.RawMessage `json:"arguments"`
		Instructions struct {
			Text string `json:"text"`
		} `json:"instructions"`
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(3)
	}
	if json.Unmarshal(b, &input) != nil || input.Instructions.Text == "" {
		os.Exit(4)
	}
	var mode struct {
		Mode    string `json:"mode"`
		PIDFile string `json:"pidFile"`
	}
	_ = json.Unmarshal(input.Arguments, &mode)
	codex := len(args) > 0 && args[0] == "exec"
	if codex {
		fmt.Printf("{\"type\":\"thread.started\",\"thread_id\":\"fixture-codex-%d\"}\n", os.Getpid())
	}
	if mode.Mode == "crash" {
		if !codex {
			fmt.Printf("{\"is_error\":true,\"session_id\":\"fixture-claude-%d\"}\n", os.Getpid())
		}
		fmt.Fprintln(os.Stderr, "PRIVATE_CREDENTIAL_MUST_NOT_LEAK")
		os.Exit(7)
	}
	if mode.Mode == "sleep" {
		time.Sleep(time.Hour)
	}
	if mode.Mode == "spawn-child" {
		child := exec.Command(os.Args[0], "--fixture-child")
		if err := child.Start(); err != nil {
			os.Exit(11)
		}
		if err := os.WriteFile(mode.PIDFile, []byte(fmt.Sprint(child.Process.Pid)), 0600); err != nil {
			os.Exit(12)
		}
		time.Sleep(time.Hour)
	}
	output := input.Arguments
	if mode.Mode == "invalid" {
		output = []byte(`{"wrong":true}`)
	}
	if mode.Mode == "duplicate" {
		output = []byte(`{"n":1,"n":2}`)
	}
	if mode.Mode == "permission" {
		// Observe the actual isolation flags supplied to the child. This is a
		// binding test, not proof that a real agent enforces its sandbox.
		joined := strings.Join(args, "|")
		if !(strings.Contains(joined, "--tools||--strict-mcp-config") || strings.Contains(joined, "--sandbox|read-only")) {
			os.Exit(8)
		}
	}
	if codex {
		var outPath string
		for i, a := range args {
			if a == "--output-last-message" && i+1 < len(args) {
				outPath = args[i+1]
			}
		}
		if outPath == "" || filepath.Dir(outPath) == "." {
			os.Exit(9)
		}
		if err := os.WriteFile(outPath, output, 0600); err != nil {
			os.Exit(10)
		}
		fmt.Println(`{"type":"turn.completed"}`)
	} else {
		fmt.Printf("{\"is_error\":false,\"session_id\":\"fixture-claude-%d\",\"structured_output\":%s}\n", os.Getpid(), output)
	}
}
