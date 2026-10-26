//go:build !linux && !darwin

package worker

import (
	"errors"
	"os/exec"
)

func exitObservationSupported() error {
	return errors.New("safe exit observation is supported on Linux and macOS")
}
func observeExit(cmd *exec.Cmd) error { return exitObservationSupported() }
