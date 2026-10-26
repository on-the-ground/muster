//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/on-the-ground/muster/internal/contract"
)

func TestCancelTerminatesDescendantProcessGroup(t *testing.T) {
	binary := fixture(t)
	backend, err := New(contract.Backend{Kind: "codex", Executable: binary})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	prompt, _ := json.Marshal(map[string]any{"instructions": map[string]string{"text": "Spawn fixture child."}, "arguments": map[string]string{"mode": "spawn-child", "pidFile": pidFile}})
	p, err := backend.Start(ctx, Request{Prompt: prompt, Schema: json.RawMessage(`{"type":"object"}`)})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	childPID := 0
	for childPID == 0 {
		if b, err := os.ReadFile(pidFile); err == nil {
			childPID, _ = strconv.Atoi(string(b))
		}
		if time.Now().After(deadline) {
			cancel()
			_, _ = p.Wait()
			t.Fatal("child was not started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer syscall.Kill(childPID, syscall.SIGKILL)
	cancel()
	out, err := p.Wait()
	if err == nil || !out.ParentExited || !out.CancelRequested || out.SessionID == "" {
		t.Fatalf("bad cancellation outcome: %+v %v", out, err)
	}
	if out.Terminated != groupAbsent(p.(*process).cmd) {
		t.Fatal("group termination evidence does not match observed group absence")
	}
	for {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		// PID 1 may not yet have reaped a terminated orphan. A zombie cannot
		// execute; distinguish that from a still-running descendant.
		if stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(childPID), "stat")); err == nil {
			if end := strings.LastIndexByte(string(stat), ')'); end >= 0 && strings.HasPrefix(string(stat)[end+1:], " Z ") {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("descendant still executing after cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNormalExitCleansGroupAndDistinguishesOpenPipes(t *testing.T) {
	binary := fixture(t)
	for _, mode := range []string{"leave-child", "leave-escaped-child"} {
		t.Run(mode, func(t *testing.T) {
			b, _ := New(contract.Backend{Kind: "claude", Executable: binary})
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			prompt, _ := json.Marshal(map[string]any{"instructions": map[string]string{"text": "Return while a child exists"}, "arguments": map[string]string{"mode": mode, "pidFile": pidFile}})
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			p, err := b.Start(ctx, Request{Prompt: prompt})
			if err != nil {
				t.Fatal(err)
			}
			// Ensure test cleanup can always terminate an intentionally escaped child.
			var pid int
			deadline := time.Now().Add(3 * time.Second)
			for pid == 0 {
				if data, err := os.ReadFile(pidFile); err == nil {
					pid, _ = strconv.Atoi(string(data))
				}
				if time.Now().After(deadline) {
					t.Fatal("child not created")
				}
				time.Sleep(10 * time.Millisecond)
			}
			defer syscall.Kill(pid, syscall.SIGKILL)
			out, err := p.Wait()
			if !out.ParentExited || out.CancelRequested {
				t.Fatalf("normal exit recorded as cancellation: %+v", out)
			}
			if mode == "leave-escaped-child" {
				if !errors.Is(err, ErrWaitDelay) || out.Terminated {
					t.Fatalf("open pipes not distinguished: %+v %v", out, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if out.Terminated != groupAbsent(p.(*process).cmd) {
				t.Fatal("reported unobserved process-group termination")
			}
			// The group can still contain an unreaped zombie; it must never contain
			// an executing child after normal-exit cleanup.
			if data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat")); err == nil {
				end := strings.LastIndexByte(string(data), ')')
				if end < 0 || !strings.HasPrefix(string(data)[end+1:], " Z ") {
					t.Fatal("live child survived normal CLI exit")
				}
			}
		})
	}
}
