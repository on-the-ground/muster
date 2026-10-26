package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/on-the-ground/muster/internal/contract"
	"github.com/on-the-ground/muster/internal/worker"
)

const schema = `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}`

type fakeBackend struct {
	calls        atomic.Int32
	output       json.RawMessage
	err          error
	beforeReturn func()
	uncertain    bool
}

func (b *fakeBackend) Check(context.Context) error { return nil }
func (b *fakeBackend) Start(_ context.Context, r worker.Request) (worker.Process, error) {
	b.calls.Add(1)
	return &fakeProcess{out: b.output, err: b.err, beforeReturn: b.beforeReturn, uncertain: b.uncertain}, nil
}

type fakeProcess struct {
	out          json.RawMessage
	err          error
	beforeReturn func()
	uncertain    bool
}

func (p *fakeProcess) Cancel() error { return nil }
func (p *fakeProcess) Wait() (worker.Outcome, error) {
	if p.beforeReturn != nil {
		p.beforeReturn()
	}
	return worker.Outcome{Output: p.out, Started: true, ParentExited: true, Terminated: !p.uncertain, SessionID: "test-session"}, p.err
}

type shortWriter struct{ calls int }

func (w *shortWriter) Write(b []byte) (int, error) { w.calls++; return len(b) - 1, nil }
func TestJournalLatchesFailureAndPreventsFutureDispatch(t *testing.T) {
	w := &shortWriter{}
	j := NewJournal(w)
	if j.Append(Evidence{}) == nil || j.Append(Evidence{}) == nil || w.calls != 1 {
		t.Fatal("journal appended after a damaged record")
	}
	b := &fakeBackend{}
	p, _ := New(catalog(t), map[string]worker.Backend{"test": b}, j)
	defer p.Close()
	if r := p.Call(context.Background(), "echo", json.RawMessage(`{"n":1}`)); r.Error != "evidence_failure" || b.calls.Load() != 0 {
		t.Fatal("dispatch continued with a failed journal")
	}
}

func TestObservedCompletionWinsOverLateContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &fakeBackend{output: json.RawMessage(`{"n":1}`), beforeReturn: cancel}
	var log bytes.Buffer
	p, _ := New(catalog(t), map[string]worker.Backend{"test": b}, NewJournal(&log))
	defer p.Close()
	if r := p.Call(ctx, "echo", json.RawMessage(`{"n":1}`)); r.Error != "" {
		t.Fatalf("late context cancellation changed observed completion: %s", r.Error)
	}
}

func TestUncertainTerminationAndPipeDelayAreDistinctEvidence(t *testing.T) {
	b := &fakeBackend{uncertain: true, err: worker.ErrWaitDelay}
	var log bytes.Buffer
	p, _ := New(catalog(t), map[string]worker.Backend{"test": b}, NewJournal(&log))
	defer p.Close()
	r := p.Call(context.Background(), "echo", json.RawMessage(`{"n":1}`))
	if r.Error != "effect_uncertain" || r.Evidence.WorkerTerminated || !r.Evidence.ParentExited || r.Evidence.FailureKind != "worker_io_timeout" || len(r.Output) > 0 {
		t.Fatalf("uncertain termination concealed: %+v", r)
	}
}

func TestInvalidInputExplainsFieldWithoutEchoingRawValue(t *testing.T) {
	b := &fakeBackend{}
	var log bytes.Buffer
	p, _ := New(catalog(t), map[string]worker.Backend{"test": b}, NewJournal(&log))
	defer p.Close()
	r := p.Call(context.Background(), "echo", json.RawMessage(`{"n":"PRIVATE_VALUE"}`))
	if r.Error != "invalid_input" || !strings.Contains(r.Message, `["n"]`) || !strings.Contains(r.Message, "number") || strings.Contains(r.Message, "PRIVATE_VALUE") {
		t.Fatalf("unhelpful or unsafe input error: %+v", r)
	}
}

func catalog(t *testing.T) *contract.Catalog {
	t.Helper()
	s, err := contract.CompileSchema(json.RawMessage(schema), true)
	if err != nil {
		t.Fatal(err)
	}
	return &contract.Catalog{Tools: []contract.LoadedTool{{Definition: contract.Tool{Name: "echo", Version: "1", InputSchema: json.RawMessage(schema), OutputSchema: json.RawMessage(schema), Instructions: contract.Instructions{Text: "echo input", Version: "1", SHA256: contract.Hash([]byte("echo input"))}, Binding: contract.Binding{Backend: "test"}, Execution: contract.Execution{TimeoutSeconds: 2, Resources: "captured-context"}}, ContractHash: "contract-hash", Input: s, Output: s}}}
}

func TestResultClassificationAndNoRetries(t *testing.T) {
	for _, tc := range []struct {
		name, input, output, want string
		err                       error
	}{
		{"large", `{"n":9007199254740993}`, `{"n":9007199254740993}`, "", nil},
		{"fractional", `{"n":9007199254740993.1}`, `{}`, "invalid_input", nil},
		{"duplicate input", `{"n":1,"n":2}`, `{}`, "invalid_input", nil},
		{"duplicate result", `{"n":1}`, `{"n":1,"n":2}`, "output_contract_violation", nil},
		{"output shape", `{"n":1}`, `{"wrong":1}`, "output_contract_violation", nil},
		{"crash", `{"n":1}`, `{}`, "execution_failed", errors.New("SECRET")},
		{"extract", `{"n":1}`, `{}`, "output_contract_violation", worker.ErrOutput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeBackend{output: json.RawMessage(tc.output), err: tc.err}
			var log bytes.Buffer
			p, err := New(catalog(t), map[string]worker.Backend{"test": b}, NewJournal(&log))
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			r := p.Call(context.Background(), "echo", json.RawMessage(tc.input))
			if r.Error != tc.want {
				t.Fatalf("got %q, want %q", r.Error, tc.want)
			}
			if r.Error == "" && string(r.Output) != tc.output {
				t.Fatal("output changed")
			}
			wantCalls := int32(1)
			if tc.want == "invalid_input" {
				wantCalls = 0
			}
			if b.calls.Load() != wantCalls {
				t.Fatal("unexpected dispatch or retry")
			}
			if strings.Contains(log.String(), "SECRET") || strings.Contains(log.String(), tc.input) {
				t.Fatal("raw input or error leaked to evidence")
			}
			if r.Evidence.JobID == "" || r.Evidence.FinishedAt == nil {
				t.Fatal("missing correlation")
			}
		})
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
func TestEvidenceFailurePreventsDispatch(t *testing.T) {
	b := &fakeBackend{}
	p, _ := New(catalog(t), map[string]worker.Backend{"test": b}, NewJournal(brokenWriter{}))
	defer p.Close()
	r := p.Call(context.Background(), "echo", json.RawMessage(`{"n":1}`))
	if r.Error != "evidence_failure" || b.calls.Load() != 0 {
		t.Fatal("executed without evidence")
	}
}

func TestAssemblyIsStableAndKeepsNumberSpelling(t *testing.T) {
	c := catalog(t)
	args := json.RawMessage(`{"n":1.23400e+60}`)
	a, err := Assemble(c.Tools[0], args)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Assemble(c.Tools[0], args)
	if !bytes.Equal(a, b) || !bytes.Contains(a, []byte("1.23400e+60")) {
		t.Fatal("assembly changed tokens")
	}
}

type blockingBackend struct{ started chan struct{} }

func (b *blockingBackend) Check(context.Context) error { return nil }
func (b *blockingBackend) Start(ctx context.Context, _ worker.Request) (worker.Process, error) {
	close(b.started)
	return blockingProcess{ctx: ctx}, nil
}

type blockingProcess struct{ ctx context.Context }

func (blockingProcess) Cancel() error { return nil }
func (b blockingProcess) Wait() (worker.Outcome, error) {
	<-b.ctx.Done()
	return worker.Outcome{Started: true, Terminated: true, CancelRequested: true}, b.ctx.Err()
}

func TestBusyAndShutdownDoNotDispatchAnotherWorker(t *testing.T) {
	b := &blockingBackend{started: make(chan struct{})}
	var log bytes.Buffer
	p, err := New(catalog(t), map[string]worker.Backend{"test": b}, NewJournal(&log))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan Result, 1)
	go func() { done <- p.Call(context.Background(), "echo", json.RawMessage(`{"n":1}`)) }()
	<-b.started
	if r := p.Call(context.Background(), "echo", json.RawMessage(`{"n":2}`)); r.Error != "busy" {
		t.Fatalf("unexpected concurrent result: %s", r.Error)
	}
	p.Close()
	if r := <-done; r.Error != "cancelled" || !r.Evidence.WorkerTerminated {
		t.Fatalf("active worker not terminated: %+v", r)
	}
	if r := p.Call(context.Background(), "echo", json.RawMessage(`{"n":3}`)); r.Error != "shutting_down" {
		t.Fatalf("dispatched after shutdown: %s", r.Error)
	}
}
