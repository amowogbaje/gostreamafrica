//go:build linux

package transcode

import (
	"os/exec"
	"syscall"
)

// setProcAttrs makes the kernel kill FFmpeg if the worker process dies, so no orphan keeps encoding.
func setProcAttrs(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} }
