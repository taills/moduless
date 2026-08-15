//go:build !linux

package pluginhost

import "os/exec"

// applySandbox is a no-op outside Linux.
//
// Pdeathsig, Credential and CgroupFD are Linux-only fields of
// syscall.SysProcAttr, so none of the process isolation described in the
// deployment docs is available when running Core on macOS or Windows. That is
// acceptable for local development — where plugins are trusted code the
// developer just built — but it means a non-Linux host must never be used to
// run untrusted third-party plugins.
//
// SandboxAvailable lets startup surface that fact instead of silently
// pretending the isolation is in place.
func applySandbox(cmd *exec.Cmd, spec LaunchSpec) {}

// SandboxAvailable reports whether process-level plugin isolation can be
// enforced on this platform.
func SandboxAvailable() bool { return false }
