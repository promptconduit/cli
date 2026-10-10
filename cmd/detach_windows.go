//go:build windows

package cmd

import "os/exec"

// detachProcess is a no-op on Windows: a started child already outlives the
// parent there, and there is no setsid equivalent to apply.
func detachProcess(c *exec.Cmd) {}
