//go:build !unix

package main

import "os/exec"

func escapeChild(cmd *exec.Cmd) {}
