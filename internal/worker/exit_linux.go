//go:build linux

package worker

import (
	"errors"
	"os/exec"

	"golang.org/x/sys/unix"
)

func exitObservationSupported() error { return nil }

// WNOWAIT observes termination but reserves the PID until exec.Cmd.Wait reaps it.
func observeExit(cmd *exec.Cmd) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
