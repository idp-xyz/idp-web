//go:build !linux

package pdfread

import "os/exec"

func setProcAttr(cmd *exec.Cmd) {}
