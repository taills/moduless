//go:build linux

package pluginhost

import (
	"os/exec"
	"syscall"
)

// applyProcessAttrs sets the Linux process attributes that keep plugin
// lifetimes tied to Core's.
//
// This is process management, not sandboxing. Plugins are reviewed and
// installed by an operator and run as trusted code, so there is no uid
// downgrade and no cgroup confinement here — what remains is about not leaking
// processes.
//
// It lives behind a build tag because Pdeathsig does not exist in
// syscall.SysProcAttr outside Linux, and this project is developed on macOS.
func applyProcessAttrs(cmd *exec.Cmd, spec LaunchSpec) {
	attr := cmd.SysProcAttr
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}

	// Setpgid puts the plugin in its own process group, so Core can signal the
	// whole group including anything the plugin forked off itself.
	attr.Setpgid = true

	// Pdeathsig has the kernel kill the plugin if Core dies. Without it a Core
	// crash leaves orphaned plugin processes holding their sockets and memory,
	// and the next Core start finds them still running.
	//
	// Development skips it: air restarts Core on every rebuild, and taking
	// every plugin down with it makes the edit loop painful.
	if !spec.DevMode {
		attr.Pdeathsig = syscall.SIGKILL
	}

	cmd.SysProcAttr = attr
}
