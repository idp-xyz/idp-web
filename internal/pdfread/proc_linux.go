//go:build linux

package pdfread

import (
	"os/exec"
	"syscall"
)

// The child leads its own process group, so that the whole group is killed at the deadline, and dies with its parent.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
