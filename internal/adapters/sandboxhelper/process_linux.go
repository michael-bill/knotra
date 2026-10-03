//go:build linux

package main

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureProcess(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = time.Second
}
func protectSupervisor() error {
	// A workload shares the unprivileged UID, but must not attach to its parent
	// supervisor or modify its memory to defeat the lease watchdog.
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, 4, 0, 0, 0, 0, 0) // PR_SET_DUMPABLE=0
	if errno != 0 {
		return errno
	}
	return nil
}
func hasMultipleLinks(i os.FileInfo) bool {
	s, ok := i.Sys().(*syscall.Stat_t)
	return !ok || s.Nlink != 1
}
