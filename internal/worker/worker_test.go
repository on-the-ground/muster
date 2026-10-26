package worker

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-the-ground/muster/internal/contract"
)

func fixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker")
	cmd := exec.Command("go", "build", "-o", path, "../testworker")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	return path
}

func TestCLIAdaptersUseSeparateProcessesAndExactOutput(t *testing.T) {
	binary := fixture(t)
	for _, kind := range []string{"claude", "codex"} {
		t.Run(kind, func(t *testing.T) {
			b, err := New(contract.Backend{Kind: kind, Executable: binary})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.Check(context.Background()); err != nil {
				t.Fatal(err)
			}
			var previous string
			for range 2 {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				prompt := []byte(`{"instructions":{"text":"Echo arguments."},"arguments":{"n":9007199254740993,"d":0.1234567890123456789,"mode":"permission"}}`)
				p, err := b.Start(ctx, Request{Prompt: prompt, Schema: json.RawMessage(`{"type":"object"}`)})
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				out, err := p.Wait()
				cancel()
				_ = p.Cancel()
				again, _ := p.Wait()
				if again.CancelRequested || again.CancelReason != "" {
					t.Fatal("late cancellation changed completed worker evidence")
				}
				if err != nil {
					t.Fatal(err)
				}
				if !out.Started || !out.Terminated || out.CancelRequested || !strings.Contains(string(out.Output), "9007199254740993") || !strings.Contains(string(out.Output), "0.1234567890123456789") {
					t.Fatalf("bad outcome: %+v", out)
				}
				if out.SessionID == "" || out.SessionID == previous {
					t.Fatal("session reused or not correlated")
				}
				previous = out.SessionID
			}
		})
	}
}

func TestCancellationAndTimeoutReapCLI(t *testing.T) {
	binary := fixture(t)
	for _, kind := range []string{"claude", "codex"} {
		t.Run(kind, func(t *testing.T) {
			b, _ := New(contract.Backend{Kind: kind, Executable: binary})
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			p, err := b.Start(ctx, Request{Prompt: []byte(`{"instructions":{"text":"Wait."},"arguments":{"mode":"sleep"}}`), Schema: json.RawMessage(`{"type":"object"}`)})
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			out, err := p.Wait()
			if err == nil || !out.CancelRequested || !out.Terminated || time.Since(start) > 3*time.Second {
				t.Fatalf("not cancelled and reaped: %+v %v", out, err)
			}
			if err := p.Cancel(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFailedProbeAndCrash(t *testing.T) {
	if _, err := New(contract.Backend{Kind: "claude", Executable: "/does-not-exist"}); err == nil {
		t.Fatal("missing executable accepted")
	}
	if _, err := New(contract.Backend{Kind: "arbitrary", Executable: "sh"}); err == nil {
		t.Fatal("arbitrary backend accepted")
	}
	binary := fixture(t)
	b, _ := New(contract.Backend{Kind: "claude", Executable: binary})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p, err := b.Start(ctx, Request{Prompt: []byte(`{"instructions":{"text":"Echo."},"arguments":{"mode":"crash"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Wait()
	if err == nil || !out.Terminated || out.SessionID == "" || strings.Contains(err.Error(), "PRIVATE_CREDENTIAL") {
		t.Fatal("crash not sanitized")
	}
}
