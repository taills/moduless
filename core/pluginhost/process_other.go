//go:build !linux

package pluginhost

import "os/exec"

// applyProcessAttrs is a no-op outside Linux.
//
// Pdeathsig is a Linux-only field of syscall.SysProcAttr, so on macOS a Core
// that crashes leaves its plugin processes running. That is tolerable for
// development — the deployment target is Linux — but it is worth knowing when
// a stale plugin process turns up holding a port after a hard kill.
func applyProcessAttrs(cmd *exec.Cmd, spec LaunchSpec) {}
