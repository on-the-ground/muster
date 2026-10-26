// Package worker binds contracts to native CLI subprocesses, never a shell.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/on-the-ground/muster/internal/contract"
	"github.com/on-the-ground/muster/internal/strictjson"
)

type Request struct {
	Prompt []byte
	Schema json.RawMessage
}

type Outcome struct {
	Output          json.RawMessage
	SessionID       string
	Started         bool
	Terminated      bool
	CancelRequested bool
}

type Process interface {
	Cancel() error
	Wait() (Outcome, error)
}

type Backend interface {
	Check(context.Context) error
	Start(context.Context, Request) (Process, error)
}

type CLI struct{ kind, executable string }

func New(spec contract.Backend) (*CLI, error) {
	if spec.Kind != "claude" && spec.Kind != "codex" {
		return nil, errors.New("unknown backend")
	}
	p, err := exec.LookPath(spec.Executable)
	if err != nil {
		return nil, errors.New("configured backend executable not available")
	}
	p, err = filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	if err := supportedPlatform(); err != nil {
		return nil, err
	}
	return &CLI{kind: spec.Kind, executable: p}, nil
}

// Check refuses unsupported CLI versions before advertising any tool. Credentials
// remain the operator's native CLI configuration and are verified by a real call.
func (b *CLI) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := []string{"--bare", "--help"}
	required := []string{"--json-schema", "--output-format", "--tools", "--strict-mcp-config", "--mcp-config", "--permission-mode", "--setting-sources", "--no-session-persistence", "--disable-slash-commands", "--settings", "--bare"}
	if b.kind == "codex" {
		args = []string{"exec", "--help"}
		required = []string{"--output-schema", "--output-last-message", "--sandbox", "--ephemeral", "--ignore-user-config", "--ignore-rules"}
	}
	p, err := launch(ctx, b.executable, args, "", nil)
	if err != nil {
		return errors.New("backend capability probe failed to start")
	}
	_, err = p.Wait()
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("backend capability probe timed out")
		}
		return errors.New("backend capability probe failed")
	}
	for _, flag := range required {
		if !strings.Contains(p.stdout.String(), flag) {
			return fmt.Errorf("backend does not support required flag %s", flag)
		}
	}
	return nil
}

func (b *CLI) Start(ctx context.Context, r Request) (Process, error) {
	dir, err := os.MkdirTemp("", "muster-job-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	args := []string{}
	switch b.kind {
	case "claude":
		args = []string{"-p", "--bare", "--output-format", "json", "--json-schema", string(r.Schema), "--tools", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--setting-sources", "", "--settings", `{"disableAllHooks":true}`, "--disable-slash-commands", "--no-session-persistence", "--permission-mode", "dontAsk"}
	case "codex":
		if err := os.WriteFile(filepath.Join(dir, "schema.json"), r.Schema, 0600); err != nil {
			cleanup()
			return nil, err
		}
		args = []string{"exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check", "--sandbox", "read-only", "-c", `approval_policy="never"`, "-c", `web_search="disabled"`, "-c", `features.shell_tool=false`, "-c", `features.apply_patch=false`, "--output-schema", filepath.Join(dir, "schema.json"), "--output-last-message", filepath.Join(dir, "result.json"), "--json", "-"}
	}
	p, err := launch(ctx, b.executable, args, dir, r.Prompt)
	if err != nil {
		cleanup()
		return nil, err
	}
	p.kind, p.dir = b.kind, dir
	p.cleanup = cleanup
	return p, nil
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > strictjson.MaxBytes {
		return 0, errors.New("worker stream exceeds 4 MiB")
	}
	return b.Buffer.Write(p)
}

type process struct {
	cmd                   *exec.Cmd
	stdout, stderr        boundedBuffer
	kind, dir             string
	cleanup               func()
	done                  chan struct{}
	mu                    sync.Mutex
	cancelled, terminated bool
	waitOnce              sync.Once
	out                   Outcome
	err                   error
}

func launch(ctx context.Context, binary string, args []string, dir string, prompt []byte) (*process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := &process{cmd: exec.Command(binary, args...), done: make(chan struct{})}
	p.cmd.Dir = dir
	p.cmd.Stdin = bytes.NewReader(prompt)
	p.cmd.Stdout, p.cmd.Stderr = &p.stdout, &p.stderr
	p.cmd.WaitDelay = 2 * time.Second
	configureProcess(p.cmd)
	if err := p.cmd.Start(); err != nil {
		return nil, errors.New("worker process could not start")
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Cancel()
		case <-p.done:
		}
	}()
	return p, nil
}

// Cancel requests termination of the CLI process group. Wait separately records
// the observed exit. This is best-effort process termination, not rollback.
func (p *process) Cancel() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.terminated {
		return nil
	}
	p.cancelled = true
	return killGroup(p.cmd)
}

func (p *process) Wait() (Outcome, error) {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		p.mu.Lock()
		p.terminated = true
		p.out = Outcome{Started: true, Terminated: p.cmd.ProcessState != nil, CancelRequested: p.cancelled}
		close(p.done)
		p.mu.Unlock()
		// A crash or cancellation may still have emitted a native session ID.
		// Capture it before classifying exit status; never invent a provider ID.
		p.captureSession()
		if p.cleanup != nil {
			defer p.cleanup()
		}
		if err != nil {
			p.err = errors.New("worker exited unsuccessfully or exceeded stream limits")
			return
		}
		switch p.kind {
		case "claude":
			m, decodeErr := strictjson.Object(p.stdout.Bytes())
			if decodeErr != nil {
				p.err = ErrOutput
				return
			}
			if m["is_error"] != false {
				p.err = errors.New("worker reported execution failure")
				return
			}
			if sid, ok := m["session_id"].(string); ok {
				p.out.SessionID = safeID(sid)
			}
			if _, ok := m["structured_output"]; !ok {
				p.err = ErrOutput
				return
			}
			// Extract the original token stream, without reserializing through any.
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(p.stdout.Bytes(), &envelope); err != nil {
				p.err = ErrOutput
				return
			}
			p.out.Output = envelope["structured_output"]
		case "codex":
			for _, line := range bytes.Split(p.stdout.Bytes(), []byte("\n")) {
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}
				m, err := strictjson.Object(line)
				if err != nil {
					p.err = ErrOutput
					return
				}
				if m["type"] == "error" || m["type"] == "turn.failed" {
					p.err = errors.New("worker reported execution failure")
					return
				}
				if m["type"] == "thread.started" {
					if sid, ok := m["thread_id"].(string); ok {
						p.out.SessionID = safeID(sid)
					}
				}
			}
			f, err := os.Open(filepath.Join(p.dir, "result.json"))
			if err != nil {
				p.err = ErrOutput
				return
			}
			defer f.Close()
			p.out.Output, err = io.ReadAll(io.LimitReader(f, strictjson.MaxBytes+1))
			if err != nil || len(p.out.Output) > strictjson.MaxBytes {
				p.err = ErrOutput
			}
		}
	})
	return p.out, p.err
}

var ErrOutput = errors.New("worker output extraction violated its binding")

func (p *process) captureSession() {
	if p.kind == "claude" {
		if m, err := strictjson.Object(p.stdout.Bytes()); err == nil {
			if sid, ok := m["session_id"].(string); ok {
				p.out.SessionID = safeID(sid)
			}
		}
		return
	}
	if p.kind != "codex" {
		return
	}
	for _, line := range bytes.Split(p.stdout.Bytes(), []byte("\n")) {
		m, err := strictjson.Object(line)
		if err != nil {
			continue
		}
		if m["type"] == "thread.started" {
			if sid, ok := m["thread_id"].(string); ok {
				p.out.SessionID = safeID(sid)
			}
		}
	}
}

func safeID(s string) string {
	if len(s) > 128 {
		return ""
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return ""
		}
	}
	return s
}
