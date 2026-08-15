// Command orphanhost stands in for Core in the Pdeathsig test.
//
// It launches a real plugin with DevMode off — the production path, where the
// kernel is asked to kill the plugin if this process dies — announces that it
// is up, and then waits to be killed. Nothing here cleans up on the way out,
// which is the point: the test kills it with SIGKILL, so anything that stops
// the plugin afterwards was the kernel, not this program.
package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"

	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: orphanhost <plugin-binary>")
		os.Exit(2)
	}
	binary := os.Args[1]

	data, err := os.ReadFile(binary)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read plugin: %v\n", err)
		os.Exit(1)
	}
	sum := sha256.Sum256(data)

	inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
		Key:        "orphan",
		InstanceID: "orphan-0",
		Version:    "1.0.0",
		BinaryPath: binary,
		Checksum:   sum[:],
		HostImpl:   hostsvc.New("orphan", nil, hostsvc.Deps{Config: hostsvc.NewStaticConfig()}),
		Env:        []string{"PATH=/usr/bin:/bin"},
		Stderr:     os.Stderr,
		// The whole point: production process attributes, so Pdeathsig is set.
		DevMode: false,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "launch: %v\n", err)
		os.Exit(1)
	}
	_ = inst

	fmt.Println("READY")
	os.Stdout.Sync()

	select {} // wait to be killed
}
