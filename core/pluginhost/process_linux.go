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
	// Unconditional now. It used to be skipped in development, on the argument
	// that air restarts Core on every rebuild and taking the plugins down each
	// time makes the edit loop painful. Two measurements say that trade bought
	// nothing:
	//
	//   - A second Core does not reattach to a surviving plugin. Nothing uses
	//     go-plugin's ReattachConfig, so the new Core execs a fresh process and
	//     the old one is simply abandoned — measured, one plugin before the
	//     restart and two after.
	//   - A graceful restart, which is what air does, already drains every
	//     plugin through main.go's registry.DrainAll. So the skip only ever
	//     took effect when Core died *without* draining, which is precisely
	//     the case where an orphan is pure cost.
	//
	// go-plugin offers nothing else here: it hands the child the parent's own
	// stdin rather than a pipe (client.go: cmd.Stdin = os.Stdin), so a plugin
	// never sees EOF when Core dies and has no way to notice on its own.
	attr.Pdeathsig = syscall.SIGKILL

	cmd.SysProcAttr = attr
}
