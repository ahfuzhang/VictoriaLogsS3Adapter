//go:build !unix

package supervisor

import "os/exec"

func setSysProcAttr(_ *exec.Cmd) {}
