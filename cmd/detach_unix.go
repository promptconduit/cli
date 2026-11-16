//go:build !windows

package cmd

import (
	"os/exec"
	"syscall"
)

// detachProcess starts c in its own session (setsid), so a background child —
// e.g. a trailing auto-sync that sleeps up to a minute before uploading — is
// not in the terminal's/agent's process group and survives it exiting or
// being signalled (SIGHUP/SIGINT to the group).
func detachProcess(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setsid = true
}
