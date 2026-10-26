//go:build !unix

package worker

import (
	"errors"
	"os/exec"
)

func supportedPlatform() error {
	return errors.New("process-group cancellation requires a Unix platform")
}
func configureProcess(cmd *exec.Cmd) {}
func killGroup(cmd *exec.Cmd) error  { return cmd.Process.Kill() }
func groupAbsent(cmd *exec.Cmd) bool { return false }
