//go:build unix

package worker

import (
	"errors"
	"os/exec"
	"syscall"
)

func supportedPlatform() error       { return exitObservationSupported() }
func configureProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func killGroup(cmd *exec.Cmd) error {
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func groupAbsent(cmd *exec.Cmd) bool {
	return errors.Is(syscall.Kill(-cmd.Process.Pid, 0), syscall.ESRCH)
}
