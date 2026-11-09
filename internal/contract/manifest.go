// Package contract loads an operator-owned, immutable startup catalog.
package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/on-the-ground/muster/internal/strictjson"
)

type Manifest struct {
	Version     int                `json:"version"`
	Pool        *Pool              `json:"pool,omitempty"`
	Correlation *Correlation       `json:"correlation,omitempty"`
	Backends    map[string]Backend `json:"backends"`
	Tools       []Tool             `json:"tools"`
}

// Pool is this muster instance's credit: Workers execution slots shared by all
// of its tools, and at most QueueLimit admitted calls waiting for a slot. A
// call beyond both is refused before dispatch. There is no wait deadline;
// callers cancel. Omitted, it is one slot and no queue.
type Pool struct {
	Workers    int `json:"workers"`
	QueueLimit int `json:"queueLimit"`
}

// Correlation names request _meta keys whose string values are recorded,
// uninterpreted, in the evidence of each admitted call.
type Correlation struct {
	MetaKeys []string `json:"metaKeys"`
}

const (
	MaxWorkers      = 64
	MaxQueueLimit   = 1024
	MaxMetaKeys     = 8
	MaxMetaValueLen = 256
)

// Executable is operator configuration, never caller-controlled. No arbitrary
// per-call flags, environment variables or session identifiers are accepted.
type Backend struct {
	Kind                 string   `json:"kind"`
	Executable           string   `json:"executable"`
	EnvironmentAllowlist []string `json:"environmentAllowlist,omitempty"`
}

type Instructions struct {
	Version string `json:"version"`
	Text    string `json:"text"`
	SHA256  string `json:"sha256"`
}

type Binding struct {
	Backend      string        `json:"backend"`
	Experimental bool          `json:"experimental,omitempty"`
	Extraction   string        `json:"extraction"`
	Context      []ContextFile `json:"context"`
	// WebFetchDomains, for web-read tools only, restricts WebFetch to these
	// hosts. Empty allows any host. WebSearch is not restricted by it.
	WebFetchDomains []string `json:"webFetchDomains,omitempty"`
}

// Permission and effect profiles. A web-read tool sends requests to external
// services; that is an effect, though it is declared as reading.
const (
	PermissionNoTools = "no-tools"
	PermissionWebRead = "web-read"
	EffectReadOnly    = "read-only"
	EffectExternal    = "external-read"
	ResourcesCaptured = "captured-context"
	ResourcesWeb      = "captured-context+web"
)

type ContextFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type Execution struct {
	Permissions    string `json:"permissions"`
	Effect         string `json:"effect"`
	Cancellation   string `json:"cancellation"`
	ReuseSession   bool   `json:"reuseSession"`
	Resources      string `json:"resources"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

type Tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Version      string          `json:"version"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
	Instructions Instructions    `json:"instructions"`
	Binding      Binding         `json:"binding"`
	Execution    Execution       `json:"execution"`
}

type Context struct {
	Name       string    `json:"name"`
	Content    string    `json:"content"`
	SHA256     string    `json:"sha256"`
	ObservedAt time.Time `json:"observedAt"`
}

type LoadedTool struct {
	Definition    Tool
	ContractHash  string
	Input, Output *Schema
	Context       []Context
}

type Catalog struct {
	Backends    map[string]Backend
	Tools       []LoadedTool
	Pool        Pool
	Correlation []string
}

func Hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// Keep a single Claude schema argument comfortably below Linux MAX_ARG_STRLEN.
const MaxClaudeSchemaBytes = 64 << 10

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var metaKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,128}$`)

var domainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

func ForbiddenEnvironment(name string) bool {
	switch name {
	case "CLAUDE_CODE_SESSION_ID", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SESSION_ID", "CODEX_THREAD_ID", "CODEX_TOOL_CALL_ID":
		return true
	}
	return false
}

func Load(path string) (*Catalog, error) {
	b, err := readLimited(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := strictjson.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Version != 1 || len(m.Tools) == 0 || len(m.Tools) > 100 {
		return nil, errors.New("manifest version must be 1 with 1..100 tools")
	}
	c := &Catalog{Backends: m.Backends, Pool: Pool{Workers: 1}}
	if m.Pool != nil {
		if m.Pool.Workers < 1 || m.Pool.Workers > MaxWorkers || m.Pool.QueueLimit < 0 || m.Pool.QueueLimit > MaxQueueLimit {
			return nil, fmt.Errorf("pool requires workers 1..%d and queueLimit 0..%d", MaxWorkers, MaxQueueLimit)
		}
		c.Pool = *m.Pool
	}
	if m.Correlation != nil {
		keys := map[string]bool{}
		if len(m.Correlation.MetaKeys) == 0 || len(m.Correlation.MetaKeys) > MaxMetaKeys {
			return nil, fmt.Errorf("correlation requires 1..%d metaKeys", MaxMetaKeys)
		}
		for _, k := range m.Correlation.MetaKeys {
			if !metaKeyPattern.MatchString(k) || keys[k] {
				return nil, errors.New("correlation metaKeys must be unique names of letters, digits and ._/-")
			}
			keys[k] = true
		}
		c.Correlation = m.Correlation.MetaKeys
	}
	for id, backend := range m.Backends {
		if !namePattern.MatchString(id) || (backend.Kind != "claude" && backend.Kind != "codex") || backend.Executable == "" {
			return nil, fmt.Errorf("invalid backend %q", id)
		}
		for _, name := range backend.EnvironmentAllowlist {
			if !environmentName.MatchString(name) || ForbiddenEnvironment(name) {
				return nil, fmt.Errorf("backend %s: invalid or parent-session environment name", id)
			}
		}
		if filepath.IsAbs(backend.Executable) {
			continue
		}
		if filepath.Base(backend.Executable) != backend.Executable {
			return nil, errors.New("backend executable must be a PATH name or absolute path")
		}
	}
	names := map[string]bool{}
	for _, t := range m.Tools {
		if !namePattern.MatchString(t.Name) || names[t.Name] || t.Description == "" || t.Version == "" {
			return nil, errors.New("tools require unique valid names, descriptions and versions")
		}
		names[t.Name] = true
		if t.Instructions.Version == "" || t.Instructions.Text == "" || t.Instructions.SHA256 != Hash([]byte(t.Instructions.Text)) {
			return nil, fmt.Errorf("tool %s: instruction version/content hash missing or incorrect", t.Name)
		}
		backend, ok := m.Backends[t.Binding.Backend]
		if !ok {
			return nil, fmt.Errorf("tool %s: unknown backend", t.Name)
		}
		if err := validateExecution(t, backend); err != nil {
			return nil, fmt.Errorf("tool %s: %w", t.Name, err)
		}
		input, err := CompileSchema(t.InputSchema, false)
		if err != nil {
			return nil, fmt.Errorf("tool %s input schema: %w", t.Name, err)
		}
		output, err := CompileSchema(t.OutputSchema, true)
		if err != nil {
			return nil, fmt.Errorf("tool %s output schema: %w", t.Name, err)
		}
		canonical, err := json.Marshal(struct {
			Tool    Tool
			Backend Backend
		}{t, backend})
		if err != nil {
			return nil, err
		}
		loaded := LoadedTool{Definition: t, ContractHash: Hash(canonical), Input: input, Output: output}
		seenContext := map[string]bool{}
		for _, spec := range t.Binding.Context {
			if spec.Name == "" || seenContext[spec.Name] {
				return nil, errors.New("context names must be nonempty and unique")
			}
			seenContext[spec.Name] = true
			p := spec.Path
			if !filepath.IsAbs(p) {
				p = filepath.Join(filepath.Dir(path), p)
			}
			content, err := readLimited(p)
			if err != nil {
				return nil, fmt.Errorf("context %s: %w", spec.Name, err)
			}
			// Requiring UTF-8 is part of the text-context contract.
			if !utf8.Valid(content) {
				return nil, errors.New("context must be UTF-8")
			}
			loaded.Context = append(loaded.Context, Context{Name: spec.Name, Content: string(content), SHA256: Hash(content), ObservedAt: time.Now().UTC()})
		}
		c.Tools = append(c.Tools, loaded)
	}
	return c, nil
}

func validateExecution(t Tool, backend Backend) error {
	e := t.Execution
	if e.ReuseSession || e.Cancellation != "terminate-process-group" || e.TimeoutSeconds < 1 || e.TimeoutSeconds > 3600 {
		return errors.New("require fresh sessions, terminate-process-group cancellation and timeoutSeconds 1..3600")
	}
	if len(t.Binding.WebFetchDomains) > 0 && e.Permissions != PermissionWebRead {
		return errors.New("webFetchDomains applies to web-read tools only")
	}
	seen := map[string]bool{}
	for _, d := range t.Binding.WebFetchDomains {
		if len(d) > 253 || !domainPattern.MatchString(d) || seen[d] {
			return errors.New("webFetchDomains must be unique lower-case host names")
		}
		seen[d] = true
	}
	switch backend.Kind {
	case "claude":
		if len(t.OutputSchema) > MaxClaudeSchemaBytes {
			return errors.New("Claude outputSchema exceeds the 64 KiB argument limit")
		}
		if t.Binding.Extraction != "claude-structured-output" {
			return errors.New("Claude requires claude-structured-output")
		}
		switch {
		case e.Permissions == PermissionNoTools && e.Effect == EffectReadOnly && e.Resources == ResourcesCaptured:
		case e.Permissions == PermissionWebRead && e.Effect == EffectExternal && e.Resources == ResourcesWeb:
		default:
			return errors.New("Claude requires no-tools/read-only/captured-context or web-read/external-read/captured-context+web")
		}
	case "codex":
		if e.Effect != EffectReadOnly || !t.Binding.Experimental || e.Permissions != "read-only-sandbox" || e.Resources != "host-filesystem-readable" || t.Binding.Extraction != "codex-output-file" {
			return errors.New("Codex requires read-only, experimental:true, read-only-sandbox, host-filesystem-readable and codex-output-file")
		}
	}
	return nil
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > strictjson.MaxBytes {
		return nil, errors.New("expected regular file up to 4 MiB")
	}
	// Bound reads even if a file grows after Stat.
	data, err := io.ReadAll(io.LimitReader(f, strictjson.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > strictjson.MaxBytes {
		return nil, errors.New("file exceeds 4 MiB")
	}
	return data, nil
}
