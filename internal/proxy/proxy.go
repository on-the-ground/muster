// Package proxy owns job correlation and the contract boundary. It has no
// dependency on a particular orchestrator, scheduler, or language.
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/on-the-ground/muster/internal/contract"
	"github.com/on-the-ground/muster/internal/strictjson"
	"github.com/on-the-ground/muster/internal/worker"
)

type ContextEvidence struct {
	Name       string    `json:"name"`
	SHA256     string    `json:"sha256"`
	ObservedAt time.Time `json:"observedAt"`
}

// Evidence contains correlation and digests, never raw inputs, outputs, worker
// streams, prompts, environment variables or credentials.
type Evidence struct {
	Version            int               `json:"version"`
	Event              string            `json:"event"`
	JobID              string            `json:"jobId"`
	Tool               string            `json:"tool"`
	ToolVersion        string            `json:"toolVersion"`
	ContractHash       string            `json:"contractHash"`
	InstructionVersion string            `json:"instructionVersion"`
	InstructionHash    string            `json:"instructionHash"`
	Backend            string            `json:"backend"`
	SessionID          string            `json:"sessionId,omitempty"`
	RequestHash        string            `json:"requestHash,omitempty"`
	ArgumentsHash      string            `json:"argumentsHash"`
	ResultHash         string            `json:"resultHash,omitempty"`
	Context            []ContextEvidence `json:"context"`
	Resources          string            `json:"resources"`
	StartedAt          time.Time         `json:"startedAt"`
	FinishedAt         *time.Time        `json:"finishedAt,omitempty"`
	WorkerStarted      bool              `json:"workerStarted"`
	CancelRequested    bool              `json:"cancelRequested"`
	WorkerTerminated   bool              `json:"workerTerminated"`
	Status             string            `json:"status"`
	Effect             string            `json:"effect"`
}

type Journal struct {
	mu  sync.Mutex
	out io.Writer
}

func NewJournal(out io.Writer) *Journal { return &Journal{out: out} }
func (j *Journal) Append(e Evidence) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	n, err := j.out.Write(b)
	if err != nil {
		return err
	}
	if n != len(b) {
		return io.ErrShortWrite
	}
	if s, ok := j.out.(interface{ Sync() error }); ok {
		return s.Sync()
	}
	return nil
}

type Result struct {
	Output   json.RawMessage
	Evidence Evidence
	Error    string
}

type Proxy struct {
	tools    map[string]contract.LoadedTool
	backends map[string]worker.Backend
	journal  *Journal
	slot     chan struct{}
	mu       sync.Mutex
	closing  bool
	active   map[string]context.CancelFunc
	wg       sync.WaitGroup
}

func New(c *contract.Catalog, backends map[string]worker.Backend, journal *Journal) (*Proxy, error) {
	if journal == nil || journal.out == nil {
		return nil, errors.New("evidence journal is required")
	}
	p := &Proxy{tools: map[string]contract.LoadedTool{}, backends: backends, journal: journal, slot: make(chan struct{}, 1), active: map[string]context.CancelFunc{}}
	for _, t := range c.Tools {
		if backends[t.Definition.Binding.Backend] == nil {
			return nil, errors.New("backend is not prepared")
		}
		p.tools[t.Definition.Name] = t
	}
	return p, nil
}

// Assemble is deterministic for a loaded catalog and the exact arguments JSON.
// Context is explicitly labelled data, not a source of new task conditions.
func Assemble(t contract.LoadedTool, args json.RawMessage) ([]byte, error) {
	request := struct {
		Tool         string                `json:"tool"`
		Version      string                `json:"version"`
		Instructions contract.Instructions `json:"instructions"`
		Arguments    json.RawMessage       `json:"arguments"`
		OutputSchema json.RawMessage       `json:"outputSchema"`
		Context      []contract.Context    `json:"contextData"`
	}{t.Definition.Name, t.Definition.Version, t.Definition.Instructions, args, t.Definition.OutputSchema, t.Context}
	b, err := json.Marshal(request)
	if len(b) > strictjson.MaxBytes {
		return nil, errors.New("assembled request exceeds 4 MiB")
	}
	return b, err
}

func (p *Proxy) Call(ctx context.Context, name string, args json.RawMessage) Result {
	t, ok := p.tools[name]
	if !ok {
		return Result{Error: "unknown_tool"}
	}
	// MCP arguments may be omitted for tools with no required inputs.
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	id := newID()
	e := Evidence{Version: 1, Event: "job-created", JobID: id, Tool: name, ToolVersion: t.Definition.Version, ContractHash: t.ContractHash, InstructionVersion: t.Definition.Instructions.Version, InstructionHash: t.Definition.Instructions.SHA256, Backend: t.Definition.Binding.Backend, ArgumentsHash: contract.Hash(args), Context: []ContextEvidence{}, Resources: t.Definition.Execution.Resources, StartedAt: time.Now().UTC(), Status: "accepted", Effect: "not-started"}
	for _, c := range t.Context {
		e.Context = append(e.Context, ContextEvidence{c.Name, c.SHA256, c.ObservedAt})
	}
	finish := func(code string, output json.RawMessage) Result {
		now := time.Now().UTC()
		e.Event, e.Status, e.FinishedAt = "job-finished", code, &now
		if len(output) > 0 {
			e.ResultHash = contract.Hash(output)
		}
		if err := p.journal.Append(e); err != nil {
			code = "evidence_failure"
			output = nil
			e.Status = code
		}
		if code == "succeeded" {
			return Result{Output: output, Evidence: e}
		}
		return Result{Evidence: e, Error: code}
	}
	// A terminal-only record is sufficient for requests rejected before dispatch.
	input, err := strictjson.Object(args)
	if err != nil || t.Input.Validate(input) != nil {
		return finish("invalid_input", nil)
	}
	prompt, err := Assemble(t, args)
	if err != nil {
		return finish("invalid_input", nil)
	}
	e.RequestHash = contract.Hash(prompt)
	select {
	case p.slot <- struct{}{}:
		defer func() { <-p.slot }()
	default:
		return finish("busy", nil)
	}
	jobctx, cancel := context.WithTimeout(ctx, time.Duration(t.Definition.Execution.TimeoutSeconds)*time.Second)
	defer cancel()
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return finish("shutting_down", nil)
	}
	p.active[id] = cancel
	p.wg.Add(1)
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.active, id); p.mu.Unlock(); p.wg.Done() }()
	if err := p.journal.Append(e); err != nil {
		return Result{Error: "evidence_failure", Evidence: e}
	}
	if err := jobctx.Err(); err != nil {
		return finish("cancelled", nil)
	}
	proc, err := p.backends[t.Definition.Binding.Backend].Start(jobctx, worker.Request{Prompt: prompt, Schema: t.Definition.OutputSchema})
	if err != nil {
		if jobctx.Err() != nil {
			e.CancelRequested = true
			return finish("cancelled", nil)
		}
		return finish("execution_failed", nil)
	}
	e.WorkerStarted, e.Effect = true, "read-only-requested"
	e.Event = "worker-started"
	if err := p.journal.Append(e); err != nil {
		_ = proc.Cancel()
		out, _ := proc.Wait()
		e.WorkerTerminated, e.CancelRequested = out.Terminated, out.CancelRequested
		return finish("evidence_failure", nil)
	}
	out, waitErr := proc.Wait()
	e.WorkerStarted, e.WorkerTerminated, e.CancelRequested, e.SessionID = out.Started, out.Terminated, out.CancelRequested, out.SessionID
	e.Effect = "read-only-no-rollback-claim"
	if !out.Terminated {
		e.Effect = "uncertain"
		return finish("effect_uncertain", nil)
	}
	if errors.Is(jobctx.Err(), context.DeadlineExceeded) {
		return finish("timeout", nil)
	}
	if errors.Is(jobctx.Err(), context.Canceled) || out.CancelRequested {
		return finish("cancelled", nil)
	}
	if errors.Is(waitErr, worker.ErrOutput) {
		return finish("output_contract_violation", nil)
	}
	if waitErr != nil {
		return finish("execution_failed", nil)
	}
	output, err := strictjson.Object(out.Output)
	if err != nil || t.Output.Validate(output) != nil {
		return finish("output_contract_violation", nil)
	}
	return finish("succeeded", out.Output)
}

func (p *Proxy) Close() {
	p.mu.Lock()
	p.closing = true
	for _, cancel := range p.active {
		cancel()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("job identity entropy: %v", err))
	}
	return hex.EncodeToString(b[:])
}
