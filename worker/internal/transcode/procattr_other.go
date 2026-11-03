//go:build !linux

package transcode

import "os/exec"

func setProcAttrs(*exec.Cmd) {}
