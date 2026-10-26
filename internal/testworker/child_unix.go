//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

func escapeChild(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
