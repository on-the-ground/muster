//go:build darwin

package worker

import (
	"errors"
	"os/exec"

	"golang.org/x/sys/unix"
)

func exitObservationSupported() error { return nil }

// NOTE_EXIT observes without reaping. A child remains ours until Cmd.Wait.
func observeExit(cmd *exec.Cmd) error {
	fd, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	changes := []unix.Kevent_t{{Ident: uint64(cmd.Process.Pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}}
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(fd, changes, events, nil)
		changes = nil
		if errors.Is(err, unix.EINTR) {
			continue
		}
		// The child may already have exited before registration; no code has
		// reaped it or released its identity yet.
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		if err != nil {
			return err
		}
		if n > 0 {
			if events[0].Flags&unix.EV_ERROR != 0 {
				err := unix.Errno(events[0].Data)
				if errors.Is(err, unix.ESRCH) {
					return nil
				}
				return err
			}
			return nil
		}
	}
}
