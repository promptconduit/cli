//go:build !windows

package cmd

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestDetachProcessStartsNewSession(t *testing.T) {
	// The child prints its own process group; with setsid it leads a new
	// session and group, so it differs from ours.
	c := exec.Command("/bin/sh", "-c", "ps -o pgid= -p $$")
	detachProcess(c)
	out, err := c.Output()
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	childPgid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse pgid %q: %v", out, err)
	}
	if childPgid == syscall.Getpgrp() {
		t.Fatalf("child shares our process group %d; expected its own session", childPgid)
	}
	if c.SysProcAttr == nil || !c.SysProcAttr.Setsid {
		t.Fatal("Setsid not set")
	}
}
