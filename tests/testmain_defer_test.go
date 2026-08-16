package tests

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// `defer` inside TestMain never runs, and nothing says so.
//
// os.Exit terminates the process without unwinding, so a TestMain shaped like
//
//	defer os.RemoveAll(dir)
//	…
//	os.Exit(m.Run())
//
// silently drops the cleanup. The code looks right — the defer is plainly
// there — and no test ever fails because of it. This repository had the bug in
// two places at once and had leaked 12.2GB of built fixtures into the
// temporary directory before anyone looked:
//
//	tests/plugin_e2e_test.go      516 dirs   9.0GB
//	core/pluginhost/launch_test.go 185 dirs   3.2GB
//
// Fixing both instances does not fix the class, so this is the guard. The
// correct shape puts the work in a helper returning int, leaving os.Exit alone
// at the top:
//
//	func TestMain(m *testing.M) { os.Exit(run(m)) }
//	func run(m *testing.M) int  { defer cleanup(); return m.Run() }
//
// The rule is deliberately coarse — any defer plus any os.Exit in the same
// TestMain — because a correct TestMain has no defers at all, and the remedy is
// the same cheap extraction every time. Being strict costs nothing; missing one
// costs gigabytes nobody notices.
func TestNoTestMainDropsItsDefers(t *testing.T) {
	root := ".."

	var offenders []string
	var scanned, found int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || name == ".git" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++

		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "TestMain" || fn.Body == nil {
				continue
			}
			found++
			if defersAndExits(fn.Body) {
				offenders = append(offenders, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking: %v", err)
	}

	// This detector reads Go source and reports nothing when it works, which is
	// also what it reports when it is broken — a scan that silently matches
	// nothing is the failure mode this repository has already been bitten by.
	// So prove it can still see, on both answers, before trusting the sweep.
	assertDetector(t, `package p
		func TestMain(m *testing.M) {
			defer os.RemoveAll(dir)
			os.Exit(m.Run())
		}`, true)
	assertDetector(t, `package p
		func TestMain(m *testing.M) { os.Exit(run(m)) }`, false)

	// Calibrating the predicate is not enough: it proves the judgement works,
	// not that the walk reached anything to judge. A wrong root or a stray
	// SkipDir would leave offenders empty and the canaries green — passing for
	// the one reason that means nothing. So assert the sweep's own reach.
	if found < 2 {
		t.Fatalf("found %d TestMain in %d test files under %s; the walk is not reaching them",
			found, scanned, root)
	}
	t.Logf("checked %d TestMain across %d test files", found, scanned)

	if len(offenders) > 0 {
		t.Errorf("TestMain drops its deferred cleanups in %v\n"+
			"os.Exit does not unwind. Move the body into a func returning int "+
			"and leave os.Exit(run(m)) as the only statement.", offenders)
	}
}

// defersAndExits reports whether the body contains both a defer and a call to
// os.Exit — the combination that guarantees the defer is dead code.
func defersAndExits(body *ast.BlockStmt) bool {
	var hasDefer, hasExit bool
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.DeferStmt:
			hasDefer = true
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" && sel.Sel.Name == "Exit" {
					hasExit = true
				}
			}
		}
		return true
	})
	return hasDefer && hasExit
}

func assertDetector(t *testing.T, src string, want bool) {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "canary.go", src, 0)
	if err != nil {
		t.Fatalf("parsing the canary: %v", err)
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "TestMain" {
			if got := defersAndExits(fn.Body); got != want {
				t.Fatalf("the detector is broken: on a %v canary it answered %v", want, got)
			}
			return
		}
	}
	t.Fatal("the canary has no TestMain; the calibration itself is wrong")
}
