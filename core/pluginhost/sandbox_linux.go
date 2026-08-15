//go:build linux

package pluginhost

import (
	"os/exec"
	"syscall"
)

// applySandbox applies the Linux-only process isolation knobs.
//
// Pdeathsig and Credential do not exist in syscall.SysProcAttr on darwin or
// windows, which is why this lives behind a build tag: the project is
// developed on macOS and deployed on Linux, and referencing those fields
// unconditionally would break the local build.
//
// Phase 6 extends this with Credential (dedicated low-privilege uid) and
// CgroupFD (atomic cgroup v2 placement at fork time, avoiding the race window
// of writing cgroup.procs after Start).
func applySandbox(cmd *exec.Cmd, spec LaunchSpec) {
	attr := cmd.SysProcAttr
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}

	// Setpgid puts the plugin in its own process group so Core can signal the
	// whole group, including any process the plugin forks off itself.
	attr.Setpgid = true

	// Pdeathsig guarantees the kernel kills the plugin if Core dies, rather
	// than leaving an orphan holding resources. This is the main reason the
	// deployment target is Linux.
	if !spec.DevMode {
		attr.Pdeathsig = syscall.SIGKILL
	}

	cmd.SysProcAttr = attr
}

// SandboxAvailable reports whether process-level plugin isolation can be
// enforced on this platform.
func SandboxAvailable() bool { return true }
