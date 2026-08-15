package tests

import (
	"github.com/taills/moduless/core/hostsvc"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Architectural invariants — the ones that are true today because someone
// arranged the packages carefully, and would stop being true the first time
// somebody adds a convenient import.
//
// These are cheap to check and expensive to discover the hard way: by the time
// a third-party author reports that building their plugin drags in Core's
// database layer, the import that caused it is several releases old.

// internalDeps returns the repo's own packages that pkg depends on.
func internalDeps(t *testing.T, pkg string) []string {
	t.Helper()

	cmd := exec.Command("go", "list", "-deps", modulePath+"/"+pkg)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("go list -deps %s: %v %s", pkg, err, stderr)
	}

	var deps []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(line, modulePath) {
			deps = append(deps, strings.TrimPrefix(line, modulePath+"/"))
		}
	}
	return deps
}

const modulePath = "github.com/taills/moduless"

// A third-party plugin depends on the SDK. If the SDK reaches into core/, then
// so does every plugin ever written against it: their builds pull in Core's
// database layer, its gateway, its auth, and they inherit a version constraint
// on all of it. The whole point of the pluginapi/ package existing separately
// is to keep that from happening.
func TestSDKDoesNotDependOnCore(t *testing.T) {
	for _, pkg := range []string{
		"sdk/plugin",
		"pluginapi",
		"proto/plugin",
	} {
		t.Run(pkg, func(t *testing.T) {
			deps := internalDeps(t, pkg)
			t.Logf("%s depends on %v", pkg, deps)

			for _, dep := range deps {
				if strings.HasPrefix(dep, "core/") {
					t.Errorf("%s depends on %s; a plugin author would have to build all of Core "+
						"to build their plugin", pkg, dep)
				}
			}
		})
	}
}

// The examples are what an author copies. If one of them reaches into core/,
// the pattern spreads by imitation even if the SDK itself stays clean.
func TestExamplesDoNotDependOnCore(t *testing.T) {
	for _, pkg := range []string{
		"extension-example/notes",
		"extension-example/ratelimit",
		"extension-example/audit",
	} {
		t.Run(pkg, func(t *testing.T) {
			for _, dep := range internalDeps(t, pkg) {
				if strings.HasPrefix(dep, "core/") {
					t.Errorf("the %s example depends on %s; plugins must build against the SDK alone", pkg, dep)
				}
			}
		})
	}
}

// Core is allowed to depend on anything, but it must still go through the same
// contract a plugin does rather than reaching into the SDK's internals. A Core
// that imported sdk/plugin would be coupling the host to the client library,
// and the two are versioned independently by design — a plugin built against
// last year's SDK has to keep working.
func TestCoreDoesNotDependOnTheSDK(t *testing.T) {
	deps := internalDeps(t, "core")
	t.Logf("core depends on %v", deps)

	for _, dep := range deps {
		if strings.HasPrefix(dep, "sdk/") {
			t.Errorf("core depends on %s; the host must not be coupled to the plugin client library", dep)
		}
	}
}

// Every capability Core can expose is actually wired up in main.go.
//
// This checks the shape of bug that has now appeared three times in this
// codebase: a capability with a complete contract on both ends and nothing
// joining them in the middle. Configuration push had it, the cron scheduler
// had it, and configuration storage had it — a manifest could declare
// settings, Core merged the defaults, SetConfig could deliver a change, and no
// value could ever be set because main.go supplied an empty in-memory store.
//
// What each had in common is that both halves were tested and the join was
// not. A nil field in Deps is not an error anywhere: the capability simply
// reports Unavailable forever, which is indistinguishable from a Core
// deliberately started without a database.
func TestEveryHostCapabilityIsWiredInMain(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join("..", "core", "main.go"), nil, 0)
	if err != nil {
		t.Fatalf("parsing core/main.go: %v", err)
	}

	// Names main.go assigns, from both `Field: value` in a composite literal
	// and `x.Field = value`. Parsed rather than grepped so a field named only
	// in a comment does not count as wired.
	assigned := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.KeyValueExpr:
			if id, ok := v.Key.(*ast.Ident); ok {
				assigned[id.Name] = true
			}
		case *ast.AssignStmt:
			for _, lhs := range v.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok {
					assigned[sel.Sel.Name] = true
				}
			}
		}
		return true
	})

	deps := reflect.TypeOf(hostsvc.Deps{})
	for i := range deps.NumField() {
		name := deps.Field(i).Name
		if !assigned[name] {
			t.Errorf("hostsvc.Deps.%s is never assigned in core/main.go; the capability would "+
				"report Unavailable forever, which looks exactly like a Core started without a database", name)
		}
	}
}
