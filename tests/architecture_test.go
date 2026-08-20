package tests

import (
	"errors"
	"fmt"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/manifest"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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

// MaxTrackedFileBytes is what a source file is allowed to weigh.
//
// Generous: the largest legitimate file in this repository is a generated
// protobuf stub in the low hundreds of kilobytes.
const MaxTrackedFileBytes = 2 << 20

// No build output in the repository.
//
// Four compiled plugin binaries — 18MB each, 75MB together — were committed
// and pushed over four rounds of this work. `go build ./extension-example/notes`
// with no -o writes ./notes into the working directory, and a `git add -A`
// takes it from there. Nothing complained: the build was fine, the tests were
// fine, and git does not mind what it is given.
//
// Binaries in git are close to unfixable after the fact. Removing them stops
// the growth but does not recover the space — that needs a history rewrite and
// a force push, which is a decision for whoever owns the repository, not a
// cleanup. So this checks the thing that is still preventable: that no more
// arrive.
func TestNoBuildOutputIsTracked(t *testing.T) {
	// Rooted explicitly. Run from this package's directory, `git ls-files`
	// scopes to it — so the first version of this test inspected tests/** and
	// nothing else, and passed happily with an 18MB binary sitting at the
	// repository root, which is the only place they ever land.
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = ".."
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("git ls-files: %v", err)
	}

	var offenders []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" {
			continue
		}
		info, err := os.Stat(filepath.Join("..", name))
		if err != nil {
			continue // deleted in the working tree, or a submodule
		}
		if info.Size() > MaxTrackedFileBytes {
			offenders = append(offenders,
				fmt.Sprintf("%s (%.1f MB)", name, float64(info.Size())/(1<<20)))
		}
	}

	if len(offenders) > 0 {
		t.Errorf("tracked files over %d MB, which is what build output looks like:\n  %s",
			MaxTrackedFileBytes>>20, strings.Join(offenders, "\n  "))
	}
}

// The names `go build` produces without -o are ignored, so the next one does
// not get committed either. The check above catches a binary that is already
// tracked; this one catches the gap that let it happen.
func TestBuildOutputNamesAreIgnored(t *testing.T) {
	// Every package that produces a binary, and the file name it lands on.
	//
	// Discovered rather than listed: this had six names when there were seven
	// examples, so `go build ./extension-example/syncer` was leaving something
	// in the working tree that nothing here objected to. echoplugin is added
	// on top because it is a fixture rather than an example.
	names := append(shippedExamples(t), "echoplugin")
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			// Three outcomes, not two. git check-ignore exits 0 for ignored and
			// 1 for not — but it also exits 127 when git is missing and 128
			// outside a repository, and reading either of those as "not
			// ignored" reports the thing being checked as broken when the
			// check simply could not run. That is what happened in the Linux
			// container: golang:alpine ships no git, and this test failed
			// there claiming the build outputs were unignored.
			cmd := exec.Command("git", "check-ignore", "-q", filepath.Join("..", name))
			out, err := cmd.CombinedOutput()
			if err == nil {
				return // ignored, which is what we want
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Skipf("cannot run git here (%v), so this cannot be checked", err)
			}
			if exit.ExitCode() != 1 {
				t.Skipf("git could not answer (exit %d): %s", exit.ExitCode(), out)
			}
			t.Errorf("./%s is not ignored, so `go build ./extension-example/%s` "+
				"leaves something a `git add -A` will commit: %s", name, name, out)
		})
	}
}

// An example that puts itself in the menu has to ship the page that menu leads
// to. Core enforces this at runtime — apps_handler drops the entry when the
// package has no frontend directory — but by then the mistake is already
// packaged, and the person who notices is whoever clicked.
//
// Six of the eight examples were in exactly that state: menus declared, no
// frontend anywhere, so every extension in the console led to a page reading
// "404 page not found". The author's guide said not to do it and the examples
// did it anyway, which is the combination worth a test rather than a sentence.
//
// Reads the manifest through the real loader rather than grepping, so a menu
// declared in some other shape still counts.
func TestExamplesWithMenusShipAFrontend(t *testing.T) {
	entries, err := os.ReadDir("../extension-example")
	if err != nil {
		t.Fatalf("reading the examples: %v", err)
	}

	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join("..", "extension-example", e.Name())
		if _, err := os.Stat(filepath.Join(dir, "manifest.yaml")); err != nil {
			continue
		}

		t.Run(e.Name(), func(t *testing.T) {
			m, err := manifest.Load(filepath.Join(dir, "manifest.yaml"))
			if err != nil {
				t.Fatalf("loading the manifest: %v", err)
			}
			if len(m.Menus) == 0 {
				return // backend-only, which is a fine thing to be
			}
			checked++

			// The source of a page, not the built output: dist/ is gitignored,
			// so requiring it here would fail on a clean checkout.
			if _, err := os.Stat(filepath.Join(dir, "frontend", "package.json")); err != nil {
				t.Errorf("%s declares %d menu(s) but has no frontend/ to serve; "+
					"Core will drop the menu entry, and a plugin package built from "+
					"this directory would show nothing where the menu points",
					e.Name(), len(m.Menus))
			}
		})
	}

	// If every example became backend-only this test would pass while checking
	// nothing, and the case it exists for would be unguarded again.
	if checked == 0 {
		t.Error("no example declares a menu, so this test verified nothing")
	}
}
