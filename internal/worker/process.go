package worker

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"
)

type process struct {
	cmd             *exec.Cmd
	stdout, stderr  boundedBuffer
	kind, dir       string
	cleanup         func()
	done            chan struct{}
	mu              sync.Mutex
	settled         bool
	exitObserved    bool
	cancelRequested bool
	cancelReason    string
	out             Outcome
	err             error
}

func launch(ctx context.Context, binary string, args []string, dir string, prompt []byte, env []string) (*process, error) {
	return launchConfigured(ctx, binary, args, dir, prompt, env, "", nil)
}

func launchConfigured(ctx context.Context, binary string, args []string, dir string, prompt []byte, env []string, kind string, cleanup func()) (*process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := &process{cmd: exec.Command(binary, args...), kind: kind, dir: dir, cleanup: cleanup, done: make(chan struct{})}
	p.cmd.Dir = dir
	p.cmd.Env = env
	p.cmd.Stdin = bytes.NewReader(prompt)
	p.cmd.Stdout, p.cmd.Stderr = &p.stdout, &p.stderr
	p.cmd.WaitDelay = 2 * time.Second
	configureProcess(p.cmd)
	if err := p.cmd.Start(); err != nil {
		return nil, errors.New("worker process could not start")
	}
	go p.run(ctx)
	return p, nil
}

func (p *process) Cancel() error { return p.cancel("cancelled") }

func (p *process) cancel(reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.settled || p.exitObserved {
		return nil
	}
	p.cancelRequested = true
	if p.cancelReason == "" {
		p.cancelReason = reason
	}
	// The group leader has not been reaped, so its PID cannot be recycled.
	return killGroup(p.cmd)
}

func (p *process) run(ctx context.Context) {
	exit := make(chan error, 1)
	go func() {
		err := observeExit(p.cmd)
		p.mu.Lock()
		p.exitObserved = err == nil
		p.mu.Unlock()
		exit <- err
	}()
	var observeErr error
	select {
	case observeErr = <-exit:
	case <-ctx.Done():
		// Prefer an already observed exit over a simultaneous deadline. Once
		// settled, later context cancellation cannot change the recorded outcome.
		select {
		case observeErr = <-exit:
		default:
			reason := "cancelled"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				reason = "timeout"
			}
			_ = p.cancel(reason)
			observeErr = <-exit
		}
	}
	p.mu.Lock()
	p.settled = true
	// Clean up on every exit path, including success, while the unreaped leader
	// still reserves the PGID. Reaping first would reopen the stale-PID race.
	killErr := killGroup(p.cmd)
	cancelled, reason := p.cancelRequested, p.cancelReason
	p.mu.Unlock()
	waitErr := p.cmd.Wait()
	parentExited := p.cmd.ProcessState != nil
	gone := groupAbsent(p.cmd)
	deadline := time.Now().Add(200 * time.Millisecond)
	for !gone && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		gone = groupAbsent(p.cmd)
	}
	pipeDelay := errors.Is(waitErr, exec.ErrWaitDelay)
	p.out = Outcome{Started: true, ParentExited: parentExited, Terminated: parentExited && gone && observeErr == nil && killErr == nil && !pipeDelay, CancelRequested: cancelled, CancelReason: reason}
	output, session, parseErr := p.parseOutput()
	p.out.SessionID = session
	switch {
	case pipeDelay:
		p.err = ErrWaitDelay
	case observeErr != nil || waitErr != nil || killErr != nil:
		p.err = errors.New("worker exit or process-group cleanup failed")
	case parseErr != nil:
		p.err = parseErr
	default:
		p.out.Output = output
	}
	if p.cleanup != nil {
		p.cleanup()
	}
	close(p.done)
}

func (p *process) Wait() (Outcome, error) { <-p.done; return p.out, p.err }
