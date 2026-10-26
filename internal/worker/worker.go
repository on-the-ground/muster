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
	"regexp"
	"strings"
	"time"

	"github.com/on-the-ground/muster/internal/contract"
	"github.com/on-the-ground/muster/internal/strictjson"
)

type Request struct {
	Prompt []byte
	Schema json.RawMessage
}
type Outcome struct {
	Output                                             json.RawMessage
	SessionID                                          string
	Started, ParentExited, Terminated, CancelRequested bool
	CancelReason                                       string
}
type Process interface {
	Cancel() error
	Wait() (Outcome, error)
}
type Backend interface {
	Check(context.Context) error
	Start(context.Context, Request) (Process, error)
}
type Identity struct{ Kind, Path, Version string }
type CLI struct {
	kind, executable, version string
	environment               []string
}

func New(spec contract.Backend) (*CLI, error) {
	if spec.Kind != "claude" && spec.Kind != "codex" {
		return nil, errors.New("unknown backend")
	}
	for _, name := range spec.EnvironmentAllowlist {
		if contract.ForbiddenEnvironment(name) {
			return nil, errors.New("parent-session environment cannot be forwarded")
		}
	}
	path, err := exec.LookPath(spec.Executable)
	if err != nil {
		return nil, errors.New("configured backend executable not available")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, errors.New("configured backend executable cannot be resolved")
	}
	if err := supportedPlatform(); err != nil {
		return nil, err
	}
	return &CLI{kind: spec.Kind, executable: path, environment: workerEnvironment(spec.Kind, spec.EnvironmentAllowlist)}, nil
}

func (b *CLI) Identity() Identity {
	return Identity{Kind: b.kind, Path: b.executable, Version: b.version}
}

// Check probes isolated CLI help and version; it never claims authentication or
// behavioral permission enforcement from the presence of an option.
func (b *CLI) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := []string{"--help"}
	required := []string{"--json-schema", "--output-format", "--tools", "--safe-mode", "--strict-mcp-config", "--mcp-config", "--permission-mode", "--setting-sources", "--no-session-persistence", "--disable-slash-commands", "--settings"}
	if b.kind == "codex" {
		args = []string{"exec", "--help"}
		required = []string{"--output-schema", "--output-last-message", "--sandbox", "--ephemeral", "--ignore-user-config", "--ignore-rules"}
	}
	p, err := launch(ctx, b.executable, args, "", nil, b.environment)
	if err != nil {
		return errors.New("backend capability probe failed to start")
	}
	out, err := p.Wait()
	if err != nil || !out.Terminated {
		return errors.New("backend capability probe failed or did not terminate")
	}
	for _, flag := range required {
		if !hasFlag(p.stdout.String(), flag) {
			return fmt.Errorf("backend does not support required flag %s", flag)
		}
	}
	p, err = launch(ctx, b.executable, []string{"--version"}, "", nil, b.environment)
	if err != nil {
		return errors.New("backend version probe failed to start")
	}
	out, err = p.Wait()
	if err != nil || !out.Terminated {
		return errors.New("backend version probe failed")
	}
	version := strings.TrimSpace(p.stdout.String())
	if !regexp.MustCompile(`^[A-Za-z0-9._()+ /-]{1,128}$`).MatchString(version) {
		return errors.New("backend version has an unsupported format")
	}
	b.version = version
	return nil
}

func hasFlag(help, flag string) bool {
	// Match an option declaration, not a suffix or a mention in prose.
	pattern := `(?m)^[ \t]*(?:-[A-Za-z],[ \t]+)?` + regexp.QuoteMeta(flag) + `(?:[ \t=<,]|$)`
	return regexp.MustCompile(pattern).MatchString(help)
}

func (b *CLI) Start(ctx context.Context, r Request) (Process, error) {
	if b.kind == "claude" && len(r.Schema) > contract.MaxClaudeSchemaBytes {
		return nil, errors.New("Claude schema exceeds the argument limit")
	}
	dir, err := os.MkdirTemp("", "muster-job-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	var args []string
	switch b.kind {
	case "claude":
		args = []string{"-p", "--safe-mode", "--output-format", "json", "--json-schema", string(r.Schema), "--tools", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--setting-sources", "", "--settings", `{"disableAllHooks":true}`, "--disable-slash-commands", "--no-session-persistence", "--permission-mode", "dontAsk"}
	case "codex":
		if err := os.WriteFile(filepath.Join(dir, "schema.json"), r.Schema, 0600); err != nil {
			cleanup()
			return nil, err
		}
		args = []string{"exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check", "--sandbox", "read-only", "-c", `approval_policy="never"`, "-c", `web_search="disabled"`, "-c", `features.shell_tool=false`, "-c", `features.apply_patch=false`, "--output-schema", filepath.Join(dir, "schema.json"), "--output-last-message", filepath.Join(dir, "result.json"), "--json", "-"}
	}
	p, err := launchConfigured(ctx, b.executable, args, dir, r.Prompt, b.environment, b.kind, cleanup)
	if err != nil {
		cleanup()
		return nil, err
	}
	return p, nil
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if b.Len()+len(data) > strictjson.MaxBytes {
		return 0, errors.New("worker stream exceeds 4 MiB")
	}
	return b.Buffer.Write(data)
}

var ErrOutput = errors.New("worker output extraction violated its binding")
var ErrWaitDelay = errors.New("worker output pipes remained open after parent exit")

// parseOutput visits each provider envelope/event once. It still extracts a
// known native session on crashes/cancellation, without returning failed output.
func (p *process) parseOutput() (json.RawMessage, string, error) {
	switch p.kind {
	case "claude":
		m, err := strictjson.Fields(p.stdout.Bytes())
		if err != nil {
			return nil, "", ErrOutput
		}
		var session string
		var sid string
		_ = json.Unmarshal(m["session_id"], &sid)
		session = safeID(sid)
		var failed bool
		if raw, ok := m["is_error"]; !ok || json.Unmarshal(raw, &failed) != nil || failed {
			return nil, session, errors.New("worker reported execution failure")
		}
		output, ok := m["structured_output"]
		if !ok {
			return nil, session, ErrOutput
		}
		return output, session, nil
	case "codex":
		var session string
		var parseErr error
		for _, line := range bytes.Split(p.stdout.Bytes(), []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			m, err := strictjson.Object(line)
			if err != nil {
				parseErr = ErrOutput
				continue
			}
			if m["type"] == "thread.started" {
				if s, ok := m["thread_id"].(string); ok {
					session = safeID(s)
				}
			}
			if m["type"] == "error" || m["type"] == "turn.failed" {
				parseErr = errors.New("worker reported execution failure")
			}
		}
		if parseErr != nil {
			return nil, session, parseErr
		}
		file, err := os.Open(filepath.Join(p.dir, "result.json"))
		if err != nil {
			return nil, session, ErrOutput
		}
		defer file.Close()
		output, err := io.ReadAll(io.LimitReader(file, strictjson.MaxBytes+1))
		if err != nil || len(output) > strictjson.MaxBytes {
			return nil, session, ErrOutput
		}
		return output, session, nil
	}
	return nil, "", nil
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
