//go:build !linux && !windows

package main

import (
	"os"
	"os/exec"
)

func configureProcess(c *exec.Cmd) {}

func hasMultipleLinks(i os.FileInfo) bool { return false }

func protectSupervisor() error { return nil }
