package worker

import (
	"context"
	"encoding/json"
	"errors"
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

func startWeb(t *testing.T, binary, mode string, timeout time.Duration, domains ...string) (Outcome, error) {
	t.Helper()
	b, err := New(contract.Backend{Kind: "claude", Executable: binary})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	p, err := b.Start(ctx, Request{Prompt: []byte(`{"instructions":{"text":"Research."},"arguments":{"n":1,"mode":"` + mode + `"}}`), Schema: json.RawMessage(`{"type":"object"}`), Web: &WebAccess{FetchDomains: domains}})
	if err != nil {
		t.Fatal(err)
	}
	return p.Wait()
}

// A web-read worker runs with only the two web tools and reports each use as
// an effect with its target, completion and result hash.
func TestWebReadRecordsEachToolUseAsAnEffect(t *testing.T) {
	binary := fixture(t)
	out, err := startWeb(t, binary, "web", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Output) != `{"n":1,"mode":"web"}` || out.SessionID == "" || !out.Terminated {
		t.Fatalf("outcome %+v", out)
	}
	if len(out.Effects) != 2 {
		t.Fatalf("effects %+v", out.Effects)
	}
	search, fetch := out.Effects[0], out.Effects[1]
	if search.Tool != "WebSearch" || search.Target != "fixture query" || search.Status != "ok" || search.CompletedAt == nil || search.ResultHash == "" || len(search.ObservedURLs) != 1 || search.ObservedURLs[0] != "https://a.example/page" {
		t.Fatalf("search effect %+v", search)
	}
	if fetch.Tool != "WebFetch" || fetch.Target != "https://a.example/page" || fetch.Status != "ok" || fetch.RequestedAt.After(*fetch.CompletedAt) {
		t.Fatalf("fetch effect %+v", fetch)
	}
}

// The native permission flags are exact: only WebSearch and WebFetch exist,
// dontAsk refuses anything the allow rules do not cover, and domains narrow
// WebFetch.
func TestWebReadPermissionFlags(t *testing.T) {
	binary := fixture(t)
	t.Setenv("MUSTER_FIXTURE_EXPECT_ALLOW", "WebSearch,WebFetch")
	if _, err := startWeb(t, binary, "permission", 5*time.Second); err != nil {
		t.Fatalf("unrestricted: %v", err)
	}
	t.Setenv("MUSTER_FIXTURE_EXPECT_ALLOW", "WebSearch,WebFetch(domain:a.example),WebFetch(domain:b.example)")
	if _, err := startWeb(t, binary, "permission", 5*time.Second, "a.example", "b.example"); err != nil {
		t.Fatalf("domains: %v", err)
	}
	codex, err := New(contract.Backend{Kind: "codex", Executable: binary})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codex.Start(context.Background(), Request{Prompt: []byte(`{}`), Web: &WebAccess{}}); err == nil {
		t.Fatal("codex accepted web-read")
	}
}

// A native refusal is evidence, not a hidden success: the effect is marked
// denied and the denied tool is listed.
func TestWebReadDenialIsRecorded(t *testing.T) {
	out, err := startWeb(t, fixture(t), "deny", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Effects) != 1 || out.Effects[0].Status != "denied" || len(out.DeniedTools) != 1 || out.DeniedTools[0] != "WebFetch" {
		t.Fatalf("denial %+v %v", out.Effects, out.DeniedTools)
	}
}

// A worker that exposes or uses an undeclared tool is stopped as soon as the
// stream shows it.
func TestUndeclaredToolStopsTheWorker(t *testing.T) {
	binary := fixture(t)
	for _, mode := range []string{"extra-tool", "rogue-tool"} {
		t.Run(mode, func(t *testing.T) {
			start := time.Now()
			out, err := startWeb(t, binary, mode, 30*time.Second)
			if !errors.Is(err, ErrPolicy) || !out.CancelRequested || !out.Terminated || time.Since(start) > 5*time.Second {
				t.Fatalf("not stopped for policy: %+v %v", out, err)
			}
		})
	}
}

// Cancelling during a tool use keeps the effect, still "requested": it may
// have reached the remote service.
func TestCancelledWebReadKeepsRequestedEffect(t *testing.T) {
	out, err := startWeb(t, fixture(t), "web-sleep", 300*time.Millisecond)
	if err == nil || !out.CancelRequested || !out.Terminated {
		t.Fatalf("not cancelled: %+v %v", out, err)
	}
	if len(out.Effects) != 1 || out.Effects[0].Status != "requested" || out.Effects[0].Target != "https://slow.example/" {
		t.Fatalf("effect %+v", out.Effects)
	}
}
