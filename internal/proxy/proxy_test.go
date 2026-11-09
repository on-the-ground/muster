package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	if r := p.Call(context.Background(), "echo", json.RawMessage(`{"n":1}`), nil); r.Error != "evidence_failure" || b.calls.Load() != 0 {
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
	if r := p.Call(ctx, "echo", json.RawMessage(`{"n":1}`), nil); r.Error != "" {
		t.Fatalf("late context cancellation changed observed completion: %s", r.Error)
	}
}

func TestUncertainTerminationAndPipeDelayAreDistinctEvidence(t *testing.T) {
	b := &fakeBackend{uncertain: true, err: worker.ErrWaitDelay}
	var log bytes.Buffer
	p, _ := New(catalog(t), map[string]worker.Backend{"test": b}, NewJournal(&log))
	defer p.Close()
	r := p.Call(context.Background(), "echo", json.RawMessage(`{"n":1}`), nil)
	if r.Error != "effect_uncertain" || r.Evidence.WorkerTerminated || !r.Evidence.ParentExited || r.Evidence.FailureKind != "worker_io_timeout" || len(r.Output) > 0 {
		t.Fatalf("uncertain termination concealed: %+v", r)
	}
}

func TestInvalidInputExplainsFieldWithoutEchoingRawValue(t *testing.T) {
	b := &fakeBackend{}
	var log bytes.Buffer
	p, _ := New(catalog(t), map[string]worker.Backend{"test": b}, NewJournal(&log))
	defer p.Close()
	r := p.Call(context.Background(), "echo", json.RawMessage(`{"n":"PRIVATE_VALUE"}`), nil)
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
			r := p.Call(context.Background(), "echo", json.RawMessage(tc.input), nil)
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
	r := p.Call(context.Background(), "echo", json.RawMessage(`{"n":1}`), nil)
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

// gatedBackend runs each started worker until the test releases it, and
// records how many run at once.
type gatedBackend struct {
	mu       sync.Mutex
	started  chan json.RawMessage
	release  chan struct{}
	running  int
	maxSeen  int
	starts   int
	effects  []worker.Effect
	cancelOK bool
}

func newGated() *gatedBackend {
	return &gatedBackend{started: make(chan json.RawMessage, 16), release: make(chan struct{})}
}

func (b *gatedBackend) Check(context.Context) error { return nil }
func (b *gatedBackend) count() (starts, maxSeen int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.starts, b.maxSeen
}
func (b *gatedBackend) Start(ctx context.Context, r worker.Request) (worker.Process, error) {
	b.mu.Lock()
	b.running++
	b.starts++
	b.maxSeen = max(b.maxSeen, b.running)
	b.mu.Unlock()
	var a struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	_ = json.Unmarshal(r.Prompt, &a)
	b.started <- a.Arguments
	return &gatedProcess{b: b, ctx: ctx, out: a.Arguments}, nil
}

type gatedProcess struct {
	b   *gatedBackend
	ctx context.Context
	out json.RawMessage
}

func (p *gatedProcess) Cancel() error { return nil }
func (p *gatedProcess) Wait() (worker.Outcome, error) {
	defer func() { p.b.mu.Lock(); p.b.running--; p.b.mu.Unlock() }()
	select {
	case <-p.b.release:
		return worker.Outcome{Output: p.out, Started: true, ParentExited: true, Terminated: true, SessionID: "s", Effects: p.b.effects}, nil
	case <-p.ctx.Done():
		return worker.Outcome{Started: true, ParentExited: true, Terminated: true, CancelRequested: true, Effects: p.b.effects}, p.ctx.Err()
	}
}

func pooled(t *testing.T, workers, queue int, b worker.Backend, keys ...string) (*Proxy, *syncWriter) {
	t.Helper()
	c := catalog(t)
	c.Pool = contract.Pool{Workers: workers, QueueLimit: queue}
	c.Correlation = keys
	log := &syncWriter{}
	p, err := New(c, map[string]worker.Backend{"test": b}, NewJournal(log))
	if err != nil {
		t.Fatal(err)
	}
	return p, log
}

type syncWriter struct {
	mu sync.Mutex
	w  bytes.Buffer
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}

func events(log *syncWriter) []Evidence {
	log.mu.Lock()
	raw := append([]byte(nil), log.w.Bytes()...)
	log.mu.Unlock()
	var out []Evidence
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var e Evidence
		if json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func call(p *Proxy, ctx context.Context, n int, meta map[string]any) chan Result {
	r := make(chan Result, 1)
	go func() { r <- p.Call(ctx, "echo", json.RawMessage(fmt.Sprintf(`{"n":%d}`, n)), meta) }()
	return r
}

// Free credit pushes calls to workers at once; when every slot is busy the
// next call waits and is pulled by the first slot released (FIFO).
func TestPoolRunsUpToWorkersAndQueuesTheRestInOrder(t *testing.T) {
	b := newGated()
	p, log := pooled(t, 2, 4, b)
	defer p.Close()
	first, second := call(p, context.Background(), 1, nil), call(p, context.Background(), 2, nil)
	<-b.started
	<-b.started
	third := call(p, context.Background(), 3, nil)
	waitFor(t, func() bool { return count(log, "job-admitted", "queued") == 1 })
	fourth := call(p, context.Background(), 4, nil)
	waitFor(t, func() bool { return count(log, "job-admitted", "queued") == 2 })
	if starts, _ := b.count(); starts != 2 {
		t.Fatal("a third worker started beyond the credit")
	}
	b.release <- struct{}{}
	if got := string(<-b.started); got != `{"n":3}` {
		t.Fatalf("queue was not FIFO: %s", got)
	}
	for range 3 {
		b.release <- struct{}{}
	}
	for _, r := range []chan Result{first, second, third, fourth} {
		if res := <-r; res.Error != "" {
			t.Fatalf("pooled call failed: %+v", res)
		}
	}
	if _, most := b.count(); most != 2 {
		t.Fatalf("ran %d workers at once, credit is 2", most)
	}
	var queued []int
	for _, e := range events(log) {
		if e.Event == "job-admitted" && e.Status == "queued" {
			queued = append(queued, *e.QueuePosition)
		}
	}
	if fmt.Sprint(queued) != "[1 2]" {
		t.Fatalf("queue positions %v", queued)
	}
}

// Beyond the queue limit a call is refused before any worker starts.
func TestQueueLimitRefusesBeforeDispatch(t *testing.T) {
	b := newGated()
	p, log := pooled(t, 1, 1, b)
	defer p.Close()
	running := call(p, context.Background(), 1, nil)
	<-b.started
	queued := call(p, context.Background(), 2, nil)
	waitFor(t, func() bool { return count(log, "job-admitted", "queued") == 1 })
	r := p.Call(context.Background(), "echo", json.RawMessage(`{"n":3}`), nil)
	if r.Error != "overloaded" || r.Evidence.Effect != "not-started" || r.Evidence.AdmittedAt != nil || r.Evidence.WorkerStarted {
		t.Fatalf("overload not refused before admission: %+v", r)
	}
	b.release <- struct{}{}
	b.release <- struct{}{}
	if (<-running).Error != "" || (<-queued).Error != "" {
		t.Fatal("admitted calls did not complete")
	}
	if starts, _ := b.count(); starts != 2 {
		t.Fatalf("%d workers started", starts)
	}
}

// Cancelling a queued call removes it: no worker ever ran, and the next
// queued call still gets the slot.
func TestCancelledQueuedCallNeverDispatches(t *testing.T) {
	b := newGated()
	p, log := pooled(t, 1, 2, b)
	defer p.Close()
	running := call(p, context.Background(), 1, nil)
	<-b.started
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := call(p, ctx, 2, nil)
	waitFor(t, func() bool { return count(log, "job-admitted", "queued") == 1 })
	next := call(p, context.Background(), 3, nil)
	waitFor(t, func() bool { return count(log, "job-admitted", "queued") == 2 })
	cancel()
	r := <-cancelled
	if r.Error != "cancelled_before_dispatch" || r.Evidence.Effect != "not-started" || r.Evidence.WorkerStarted || r.Evidence.DispatchedAt != nil || r.Evidence.AdmittedAt == nil {
		t.Fatalf("queued cancellation evidence: %+v", r)
	}
	b.release <- struct{}{}
	if got := string(<-b.started); got != `{"n":3}` {
		t.Fatalf("cancelled call was dispatched: %s", got)
	}
	b.release <- struct{}{}
	if (<-running).Error != "" || (<-next).Error != "" {
		t.Fatal("remaining calls failed")
	}
	if starts, _ := b.count(); starts != 2 {
		t.Fatalf("%d workers started", starts)
	}
}

// Shutdown stops running workers and ends queued calls without dispatch.
func TestShutdownEndsQueuedCallsWithoutDispatch(t *testing.T) {
	b := newGated()
	p, log := pooled(t, 1, 2, b)
	running := call(p, context.Background(), 1, nil)
	<-b.started
	queued := call(p, context.Background(), 2, nil)
	waitFor(t, func() bool { return count(log, "job-admitted", "queued") == 1 })
	p.Close()
	if r := <-queued; r.Error != "shutting_down" || r.Evidence.WorkerStarted {
		t.Fatalf("queued call at shutdown: %+v", r)
	}
	if r := <-running; r.Error != "cancelled" {
		t.Fatalf("running call at shutdown: %+v", r)
	}
	if r := p.Call(context.Background(), "echo", json.RawMessage(`{"n":3}`), nil); r.Error != "shutting_down" {
		t.Fatalf("dispatched after shutdown: %s", r.Error)
	}
	if starts, _ := b.count(); starts != 1 {
		t.Fatalf("%d workers started", starts)
	}
}

// Configured _meta keys are recorded from admission on, so a call that never
// returns a result can still be joined to the caller's record.
func TestCorrelationIsRecordedAtAdmission(t *testing.T) {
	b := newGated()
	p, log := pooled(t, 1, 1, b, "subsea/callRef")
	defer p.Close()
	meta := map[string]any{"subsea/callRef": "call#7", "other": "ignored"}
	ctx, cancel := context.WithCancel(context.Background())
	r := call(p, ctx, 1, meta)
	<-b.started
	cancel()
	<-r
	ev := events(log)
	if len(ev) != 3 || ev[0].Event != "job-admitted" || ev[1].Event != "job-dispatched" || ev[2].Event != "job-finished" {
		t.Fatalf("stages %+v", ev)
	}
	for _, e := range ev {
		if len(e.Correlation) != 1 || e.Correlation["subsea/callRef"] != "call#7" {
			t.Fatalf("correlation %v in %s", e.Correlation, e.Event)
		}
	}
	for _, bad := range []any{"", 7, "a\nb", strings.Repeat("x", contract.MaxMetaValueLen+1)} {
		r := p.Call(context.Background(), "echo", json.RawMessage(`{"n":1}`), map[string]any{"subsea/callRef": bad})
		if r.Error != "invalid_correlation" || r.Evidence.WorkerStarted {
			t.Fatalf("%v: %+v", bad, r)
		}
	}
	if r := p.Call(context.Background(), "echo", json.RawMessage(`{"n":"x"}`), nil); r.Evidence.Correlation != nil {
		t.Fatal("absent key recorded")
	}
}

// Observed effects reach the evidence on success and on cancellation alike.
func TestEffectsAreRecordedEvenWhenTheJobIsCancelled(t *testing.T) {
	b := newGated()
	b.effects = []worker.Effect{{Tool: "WebFetch", Target: "https://a.example/", Status: "requested"}}
	p, _ := pooled(t, 1, 0, b)
	defer p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	r := call(p, ctx, 1, nil)
	<-b.started
	cancel()
	res := <-r
	if res.Error != "cancelled" || len(res.Evidence.Effects) != 1 || res.Evidence.Effects[0].Status != "requested" {
		t.Fatalf("effects lost on cancellation: %+v", res.Evidence)
	}
}

func count(log *syncWriter, event, status string) int {
	n := 0
	for _, e := range events(log) {
		if e.Event == event && e.Status == status {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// blockingWriter holds its first write until released, like a slow disk.
type blockingWriter struct {
	syncWriter
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	first := false
	b.once.Do(func() { first = true })
	if first {
		close(b.entered)
		<-b.release
	}
	return b.syncWriter.Write(p)
}

// Review of #4: Close must wait for every admitted call to write its
// terminal evidence, not only for dispatched ones.
func TestCloseWaitsForAdmittedCallsToFinishTheirEvidence(t *testing.T) {
	log := &blockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
	c := catalog(t)
	p, err := New(c, map[string]worker.Backend{"test": newGated()}, NewJournal(log))
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan Result, 1)
	go func() { result <- p.Call(context.Background(), "echo", json.RawMessage(`{"n":1}`), nil) }()
	<-log.entered // job-admitted is being written
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while an admitted call was still recording")
	case <-time.After(100 * time.Millisecond):
	}
	close(log.release)
	<-closed
	select {
	case r := <-result:
		if r.Error != "shutting_down" {
			t.Fatalf("admitted call at shutdown: %+v", r)
		}
	default:
		t.Fatal("Close returned before the admitted call finished")
	}
	var kinds []string
	for _, e := range events(&log.syncWriter) {
		kinds = append(kinds, e.Event+"/"+e.Status)
	}
	if strings.Join(kinds, ",") != "job-admitted/running,job-finished/shutting_down" {
		t.Fatalf("evidence %v", kinds)
	}
}

// The same for a call waiting in the queue when Close is called.
func TestCloseWaitsForQueuedCallsToFinishTheirEvidence(t *testing.T) {
	b := newGated()
	p, log := pooled(t, 1, 1, b)
	running := call(p, context.Background(), 1, nil)
	<-b.started
	queued := call(p, context.Background(), 2, nil)
	waitFor(t, func() bool { return count(log, "job-admitted", "queued") == 1 })
	p.Close()
	for _, r := range []chan Result{running, queued} {
		select {
		case <-r:
		default:
			t.Fatal("Close returned before an admitted call finished")
		}
	}
	if count(log, "job-finished", "shutting_down") != 1 || count(log, "job-finished", "cancelled") != 1 {
		t.Fatalf("terminal evidence missing: %+v", events(log))
	}
}
