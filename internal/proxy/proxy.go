// Package proxy owns job correlation and the contract boundary. It has no
// dependency on a particular orchestrator, scheduler, or language.
package proxy

import (
	"container/list"
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

// EvidenceVersion 2 records each job in three stages: job-admitted (with
// any correlation values), job-dispatched, job-finished. Calls refused before
// admission have only a job-finished record.
const EvidenceVersion = 2

// Evidence contains correlation and digests, never raw inputs, outputs, worker
// streams, prompts, environment variables or credentials. The exception is a
// web-read job's effect targets: queries and URLs are recorded in clear so the
// external requests can be audited.
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
	BackendKind        string            `json:"backendKind,omitempty"`
	BackendPath        string            `json:"backendPath,omitempty"`
	BackendVersion     string            `json:"backendVersion,omitempty"`
	SessionID          string            `json:"sessionId,omitempty"`
	Correlation        map[string]string `json:"correlation,omitempty"`
	QueuePosition      *int              `json:"queuePosition,omitempty"`
	RequestHash        string            `json:"requestHash,omitempty"`
	ArgumentsHash      string            `json:"argumentsHash"`
	ResultHash         string            `json:"resultHash,omitempty"`
	Context            []ContextEvidence `json:"context"`
	Resources          string            `json:"resources"`
	StartedAt          time.Time         `json:"startedAt"`
	AdmittedAt         *time.Time        `json:"admittedAt,omitempty"`
	DispatchedAt       *time.Time        `json:"dispatchedAt,omitempty"`
	FinishedAt         *time.Time        `json:"finishedAt,omitempty"`
	WorkerStarted      bool              `json:"workerStarted"`
	CancelRequested    bool              `json:"cancelRequested"`
	WorkerTerminated   bool              `json:"workerTerminated"`
	ParentExited       bool              `json:"parentExited"`
	FailureKind        string            `json:"failureKind,omitempty"`
	Status             string            `json:"status"`
	Effect             string            `json:"effect"`
	Effects            []worker.Effect   `json:"effects,omitempty"`
	AuditIncomplete    bool              `json:"auditIncomplete,omitempty"`
	DeniedTools        []string          `json:"deniedTools,omitempty"`
}

type Journal struct {
	mu      sync.Mutex
	out     io.Writer
	failure error
}

func NewJournal(out io.Writer) *Journal { return &Journal{out: out} }
func (j *Journal) Append(e Evidence) (err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failure != nil {
		return j.failure
	}
	defer func() {
		if err != nil {
			j.failure = err
		}
	}()
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
	Message  string
}

// Proxy admits calls against its credit: pool.Workers execution slots shared
// by all tools. A call takes a free slot at once; otherwise it waits in a FIFO
// queue of at most pool.QueueLimit calls and is handed the next slot that is
// released. Beyond that it is refused before dispatch.
type Proxy struct {
	tools       map[string]contract.LoadedTool
	backends    map[string]worker.Backend
	journal     *Journal
	pool        contract.Pool
	correlation []string
	mu          sync.Mutex
	running     int
	queue       *list.List // of *waiter
	closing     bool
	active      map[string]context.CancelFunc
	wg          sync.WaitGroup
}

// waiter is a queued call. ready is closed when a slot is handed to it
// (granted) or when the proxy shuts down (not granted).
type waiter struct {
	ready   chan struct{}
	granted bool
}

func New(c *contract.Catalog, backends map[string]worker.Backend, journal *Journal) (*Proxy, error) {
	if journal == nil || journal.out == nil {
		return nil, errors.New("evidence journal is required")
	}
	pool := c.Pool
	if pool.Workers < 1 {
		pool.Workers = 1
	}
	p := &Proxy{tools: map[string]contract.LoadedTool{}, backends: backends, journal: journal, pool: pool, correlation: c.Correlation, queue: list.New(), active: map[string]context.CancelFunc{}}
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

// Call runs one tool call. meta is the request's _meta; only the configured
// correlation keys are read from it.
func (p *Proxy) Call(ctx context.Context, name string, args json.RawMessage, meta map[string]any) Result {
	t, ok := p.tools[name]
	if !ok {
		return Result{Error: "unknown_tool"}
	}
	// MCP arguments may be omitted for tools with no required inputs.
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	id := newID()
	e := Evidence{Version: EvidenceVersion, Event: "job-finished", JobID: id, Tool: name, ToolVersion: t.Definition.Version, ContractHash: t.ContractHash, InstructionVersion: t.Definition.Instructions.Version, InstructionHash: t.Definition.Instructions.SHA256, Backend: t.Definition.Binding.Backend, ArgumentsHash: contract.Hash(args), Context: []ContextEvidence{}, Resources: t.Definition.Execution.Resources, StartedAt: time.Now().UTC(), Status: "accepted", Effect: "not-started"}
	if b, ok := p.backends[t.Definition.Binding.Backend].(interface{ Identity() worker.Identity }); ok {
		identity := b.Identity()
		e.BackendKind, e.BackendPath, e.BackendVersion = identity.Kind, identity.Path, identity.Version
	}
	var detail string
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
			detail = ""
		}
		if code == "succeeded" {
			return Result{Output: output, Evidence: e}
		}
		return Result{Evidence: e, Error: code, Message: detail}
	}
	// Close waits for every call it lets in until that call's terminal
	// evidence is written, whether it is refused, queued or dispatched.
	if !p.enter() {
		return finish("shutting_down", nil)
	}
	defer p.wg.Done()
	// A terminal-only record is sufficient for requests refused before admission.
	correlation, err := p.correlate(meta)
	if err != nil {
		detail = err.Error()
		return finish("invalid_correlation", nil)
	}
	e.Correlation = correlation
	input, err := strictjson.Object(args)
	if err != nil {
		detail = "arguments must be one JSON object with unique keys"
		return finish("invalid_input", nil)
	}
	if err := t.Input.Validate(input); err != nil {
		detail = err.Error()
		if len(detail) > 200 {
			detail = detail[:200] + "..."
		}
		return finish("invalid_input", nil)
	}
	prompt, err := Assemble(t, args)
	if err != nil {
		detail = "assembled request exceeds the size limit"
		return finish("invalid_input", nil)
	}
	e.RequestHash = contract.Hash(prompt)

	w, position, refusal := p.admit()
	if refusal != "" {
		return finish(refusal, nil)
	}
	admitted := time.Now().UTC()
	e.Event, e.AdmittedAt, e.QueuePosition = "job-admitted", &admitted, &position
	e.Status = "running"
	if w != nil {
		e.Status = "queued"
	}
	if err := p.journal.Append(e); err != nil {
		p.withdraw(w)
		return finish("evidence_failure", nil)
	}
	if w != nil {
		select {
		case <-w.ready:
		case <-ctx.Done():
			// A slot handed over at the same moment is released again.
			if p.withdraw(w) {
				// Never dispatched: no worker ran, so no effect happened.
				return finish("cancelled_before_dispatch", nil)
			}
		}
		p.mu.Lock()
		granted := w.granted
		p.mu.Unlock()
		if !granted {
			return finish("shutting_down", nil)
		}
	}
	defer p.release()
	if ctx.Err() != nil {
		return finish("cancelled_before_dispatch", nil)
	}
	// The execution deadline starts at dispatch; queueing is not counted.
	jobctx, cancel := context.WithTimeout(ctx, time.Duration(t.Definition.Execution.TimeoutSeconds)*time.Second)
	defer cancel()
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return finish("shutting_down", nil)
	}
	p.active[id] = cancel
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.active, id); p.mu.Unlock() }()
	request := worker.Request{Prompt: prompt, Schema: t.Definition.OutputSchema}
	if t.Definition.Execution.Permissions == contract.PermissionWebRead {
		request.Web = &worker.WebAccess{FetchDomains: t.Definition.Binding.WebFetchDomains}
	}
	effect := t.Definition.Execution.Effect
	proc, err := p.backends[t.Definition.Binding.Backend].Start(jobctx, request)
	if err != nil {
		if jobctx.Err() != nil {
			if errors.Is(jobctx.Err(), context.DeadlineExceeded) {
				return finish("timeout", nil)
			}
			return finish("cancelled", nil)
		}
		return finish("execution_failed", nil)
	}
	dispatched := time.Now().UTC()
	e.WorkerStarted, e.Effect, e.DispatchedAt = true, effect+"-requested", &dispatched
	e.Event, e.Status = "job-dispatched", "running"
	if err := p.journal.Append(e); err != nil {
		_ = proc.Cancel()
		out, _ := proc.Wait()
		e.WorkerTerminated, e.CancelRequested, e.Effects, e.AuditIncomplete = out.Terminated, out.CancelRequested, out.Effects, out.AuditIncomplete
		return finish("evidence_failure", nil)
	}
	out, waitErr := proc.Wait()
	e.WorkerStarted, e.WorkerTerminated, e.CancelRequested, e.SessionID = out.Started, out.Terminated, out.CancelRequested, out.SessionID
	e.ParentExited = out.ParentExited
	e.Effects, e.DeniedTools, e.AuditIncomplete = out.Effects, out.DeniedTools, out.AuditIncomplete
	if errors.Is(waitErr, worker.ErrWaitDelay) {
		e.FailureKind = "worker_io_timeout"
	}
	e.Effect = effect + "-no-rollback-claim"
	if !out.Terminated {
		e.Effect = "uncertain"
		return finish("effect_uncertain", nil)
	}
	if errors.Is(waitErr, worker.ErrPolicy) {
		return finish("permission_violation", nil)
	}
	if errors.Is(waitErr, worker.ErrAuditLimit) {
		return finish("audit_limit_exceeded", nil)
	}
	if out.CancelReason == "timeout" {
		return finish("timeout", nil)
	}
	if out.CancelRequested {
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

// correlate reads the configured _meta keys. A present value must be a short
// printable string: evidence that could not be joined must not run.
func (p *Proxy) correlate(meta map[string]any) (map[string]string, error) {
	var out map[string]string
	for _, k := range p.correlation {
		v, ok := meta[k]
		if !ok {
			continue
		}
		s, isString := v.(string)
		if !isString || s == "" || len(s) > contract.MaxMetaValueLen || !printable(s) {
			return nil, fmt.Errorf("_meta %q must be a printable string of 1..%d bytes", k, contract.MaxMetaValueLen)
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = s
	}
	return out, nil
}

func printable(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// admit takes a free slot (nil waiter), queues the call (its waiter and
// 1-based position), or refuses it with "overloaded" or "shutting_down".
func (p *Proxy) admit() (*waiter, int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.closing:
		return nil, 0, "shutting_down"
	case p.running < p.pool.Workers:
		p.running++
		return nil, 0, ""
	case p.queue.Len() < p.pool.QueueLimit:
		w := &waiter{ready: make(chan struct{})}
		p.queue.PushBack(w)
		return w, p.queue.Len(), ""
	}
	return nil, 0, "overloaded"
}

// withdraw undoes an admission that has not dispatched: it removes a queued
// call or releases the slot the call holds. It reports false only when the
// call was neither queued nor granted, which means shutdown closed it.
func (p *Proxy) withdraw(w *waiter) bool {
	p.mu.Lock()
	if w != nil {
		for el := p.queue.Front(); el != nil; el = el.Next() {
			if el.Value == w {
				p.queue.Remove(el)
				p.mu.Unlock()
				return true
			}
		}
		if !w.granted {
			p.mu.Unlock()
			return false
		}
	}
	p.mu.Unlock()
	p.release()
	return true
}

// release hands the slot to the oldest queued call, or frees it.
func (p *Proxy) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if el := p.queue.Front(); el != nil && !p.closing {
		w := p.queue.Remove(el).(*waiter)
		w.granted = true
		close(w.ready)
		return
	}
	p.running--
}

// enter registers a call with the shutdown wait, unless shutdown has begun.
func (p *Proxy) enter() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		return false
	}
	p.wg.Add(1)
	return true
}

// Close refuses new calls, ends queued calls without dispatch, cancels running
// workers, and returns once every call it let in has written its terminal
// evidence. The journal owner may close the journal after it returns.
func (p *Proxy) Close() {
	p.mu.Lock()
	p.closing = true
	for el := p.queue.Front(); el != nil; el = el.Next() {
		close(el.Value.(*waiter).ready)
	}
	p.queue.Init()
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
