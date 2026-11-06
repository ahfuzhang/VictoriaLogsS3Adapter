//go:build unix && !linux

package supervisor

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr puts the child into its own process group, so terminal signals
// such as Ctrl+C are delivered only to this process, which then stops the child
// gracefully.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
