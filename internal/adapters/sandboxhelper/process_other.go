//go:build !linux && !windows

package main

import (
	"os"
	"os/exec"
)

func configureProcess(_ *exec.Cmd) {}

func hasMultipleLinks(_ os.FileInfo) bool { return false }

func protectSupervisor() error { return nil }
