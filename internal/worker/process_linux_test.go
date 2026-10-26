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
	if err == nil || !out.Terminated || !out.CancelRequested || out.SessionID == "" {
		t.Fatalf("bad cancellation outcome: %+v %v", out, err)
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
