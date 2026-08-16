package tests

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/taills/moduless/manifest"
)

// The plugin guide against the SDK it documents.
//
// The guide is the only thing a third-party author reads — that is the whole
// premise of shipping an SDK separately from Core — so a name in it that does
// not exist costs somebody an afternoon. This has already happened twice while
// writing the shipped examples: reaching for `SortAsc`, which is `Sort`, and
// for a `WithBody` on a short-circuit result, which does not exist.
//
// Both errors above were *methods*, so a check that only looked at
// package-level `sdk.X` names would miss exactly the class it exists for. The
// method check below is name-only — it does not know which receiver a call was
// made on — which is weaker than a compiler and still catches every name that
// simply does not exist anywhere in the SDK.
//
// What none of this checks is arity, argument order or semantics. That is what
// compiling the examples is for.

// sdkRef matches an `sdk.Something` reference in the guide.
var sdkRef = regexp.MustCompile(`\bsdk\.([A-Z][A-Za-z0-9_]*)`)

// sdkMethodRef catches the method in sdk.Log.Gauge — the second segment, which
// sdkRef never looked at.
//
// It only ever captured the first, so every `sdk.X.Y()` in the guide was
// checked as far as X and no further: sdk.Log.Timer, a method that does not
// exist, passed because sdk.Log does. The code-fence scan covered some of
// these by accident, and anything written in a table — which is where the
// metric kinds are documented — was checked by nothing at all.
var sdkMethodRef = regexp.MustCompile(`\bsdk\.[A-Z][A-Za-z0-9_]*\.([A-Z][A-Za-z0-9_]*)`)

// exportedSDKNames collects everything sdk/plugin exports.
func exportedSDKNames(t *testing.T) map[string]bool {
	t.Helper()

	names := map[string]bool{}
	dir := filepath.Join("..", "sdk", "plugin")
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the SDK: %v", err)
	}

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					// Methods are collected too. Which receiver they belong to
					// is not tracked: the guide writes `q.Sort(...)` where `q`
					// came from somewhere earlier, so a name-only set is what
					// can honestly be compared against.
					if d.Name.IsExported() {
						names[d.Name.Name] = true
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						switch sp := spec.(type) {
						case *ast.TypeSpec:
							if sp.Name.IsExported() {
								names[sp.Name.Name] = true
							}
						case *ast.ValueSpec:
							for _, id := range sp.Names {
								if id.IsExported() {
									names[id.Name] = true
								}
							}
						}
					}
				}
			}
		}
	}
	return names
}

// Every sdk.X the guide mentions exists.
func TestGuideOnlyNamesRealSDKSymbols(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "docs", "plugin-development.md"))
	if err != nil {
		t.Fatalf("reading the guide: %v", err)
	}

	have := exportedSDKNames(t)
	if len(have) < 20 {
		t.Fatalf("only %d exported SDK names found; the parse is wrong and this test "+
			"would pass by accident", len(have))
	}

	seen := map[string]bool{}
	var missing []string
	for _, m := range sdkRef.FindAllStringSubmatch(string(guide), -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if !have[name] {
			missing = append(missing, name)
		}
	}

	if len(seen) < 15 {
		t.Fatalf("only %d sdk.X references found in the guide; the pattern is wrong", len(seen))
	}

	// The method half of sdk.X.Y, wherever it appears — prose, table or code.
	methods := 0
	for _, m := range sdkMethodRef.FindAllStringSubmatch(string(guide), -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		methods++
		if !have[name] {
			missing = append(missing, name)
		}
	}
	if methods == 0 {
		t.Fatal("no sdk.X.Y methods found in the guide; the pattern stopped matching " +
			"and this half of the check now passes by looking at nothing")
	}
	// Methods called on SDK values inside the guide's Go blocks. Restricted to
	// code fences so that prose mentioning a word followed by a bracket does
	// not become a false failure.
	for _, name := range methodsCalledInGoBlocks(string(guide)) {
		if seen[name] || have[name] {
			continue
		}
		seen[name] = true
		missing = append(missing, name)
	}

	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the guide names %d symbol(s) that do not exist in the SDK: %s\n"+
			"an author reading only the guide — which is the point of shipping an SDK — "+
			"finds this out at the compiler", len(missing), strings.Join(missing, ", "))
	}
	t.Logf("checked %d distinct references against %d exported SDK names", len(seen), len(have))
}

// methodCall matches `.Something(` on a value.
var methodCall = regexp.MustCompile(`\.([A-Z][A-Za-z0-9_]*)\(`)

// stdlibMethods are names the guide legitimately calls on things that are not
// SDK values — the standard library, mostly — and which the SDK has no reason
// to export. Listed rather than inferred, so adding one is a deliberate act.
var stdlibMethods = map[string]bool{
	"Atoi": true, "Contains": true, "Error": true, "Errorf": true, "Format": true,
	"Get": true, "HasPrefix": true, "Itoa": true, "Marshal": true, "New": true,
	"Now": true, "Printf": true, "Println": true, "Sprintf": true, "Unmarshal": true,
	"UTC": true, "Write": true, "WriteHeader": true, "Header": true, "Body": true,
	"ParseDuration": true, "TrimSpace": true, "Split": true, "Join": true,
	"NewDecoder": true, "NewEncoder": true, "Decode": true, "Encode": true,
	"Is": true, "As": true, "Add": true, "Sub": true, "Lock": true, "Unlock": true,
	"RLock": true, "RUnlock": true, "Sleep": true, "Background": true, "Since": true,
	"PathValue": true, "Query": true, "MaxBytesReader": true, "HandleFunc": true,
	"NewServeMux": true, "SetPrefix": true, "Close": true, "Read": true, "Len": true,
	"String": true, "Bytes": true, "Copy": true, "Fields": true, "Cut": true,
	"Wrap": true, "Done": true, "Err": true, "Value": true, "WithValue": true,
	"Context": true, "FormatInt": true, "Fprintf": true, "Unix": true,
	"WithCancel": true, "WithTimeout": true, "Once": true, "Do": true,
	"NewRequest": true, "WithContext": true, "NewRecorder": true, "ServeHTTP": true,
}

func methodsCalledInGoBlocks(guide string) []string {
	var out []string
	seen := map[string]bool{}
	inGo := false
	for _, line := range strings.Split(guide, "\n") {
		if strings.HasPrefix(line, "```go") {
			inGo = true
			continue
		}
		if strings.HasPrefix(line, "```") {
			inGo = false
			continue
		}
		if !inGo {
			continue
		}
		for _, m := range methodCall.FindAllStringSubmatch(line, -1) {
			name := m[1]
			if seen[name] || stdlibMethods[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// The manifest fields the guide shows are fields the manifest parser accepts.
//
// Stricter than it sounds now that unknown fields are refused: a key in the
// guide that the parser does not know is not a typo an author can shrug off,
// it stops their plugin loading.
func TestGuideOnlyShowsRealManifestFields(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "docs", "plugin-development.md"))
	if err != nil {
		t.Fatalf("reading the guide: %v", err)
	}

	fields := manifestYAMLTags(t)
	if len(fields) < 15 {
		t.Fatalf("only %d yaml tags found in the manifest package; the parse is wrong", len(fields))
	}

	// Top-level keys inside the guide's yaml blocks. Nested keys are indented
	// and vary by context, so this deliberately checks the shallow ones, which
	// are the ones an author copies wholesale.
	var missing []string
	inYAML := false
	for _, line := range strings.Split(string(guide), "\n") {
		if strings.HasPrefix(line, "```yaml") {
			inYAML = true
			continue
		}
		if strings.HasPrefix(line, "```") {
			inYAML = false
			continue
		}
		if !inYAML || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || key == "" || strings.Contains(key, " ") {
			continue
		}
		if !fields[key] {
			missing = append(missing, key)
		}
	}

	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the guide shows manifest keys the parser does not accept: %s\n"+
			"unknown fields are refused, so copying one of these stops the plugin loading",
			strings.Join(missing, ", "))
	}
}

// manifestYAMLTags collects every `yaml:"..."` tag in the manifest package.
func manifestYAMLTags(t *testing.T) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	dir := filepath.Join("..", "manifest")
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the manifest package: %v", err)
	}

	tag := regexp.MustCompile(`yaml:"([a-z_]+)`)
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				field, ok := n.(*ast.Field)
				if !ok || field.Tag == nil {
					return true
				}
				if m := tag.FindStringSubmatch(field.Tag.Value); m != nil {
					out[m[1]] = true
				}
				return true
			})
		}
	}
	return out
}

// A struct the guide shows is the struct the SDK declares.
//
// The guide gained a list of QueueMessage's field names — names only, no types
// — and the next author to read it assumed ID was a string. It is an int64,
// and that was three compile errors from one sentence. A prose field list
// invites a type guess; showing the declaration does not.
//
// So this checks the declarations the guide shows against the real ones: every
// field the guide lists must exist on that type with that type. It does not
// require the guide to show every field — a doc may reasonably show the
// interesting half — only that what it does show is true.
func TestGuideStructsMatchTheSDK(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "docs", "plugin-development.md"))
	if err != nil {
		t.Fatalf("reading the guide: %v", err)
	}

	real := sdkStructFields(t)
	shown := structsInGoBlocks(t, string(guide))
	if len(shown) == 0 {
		t.Skip("the guide shows no struct declarations")
	}

	checked := 0
	for name, fields := range shown {
		actual, ok := real[name]
		if !ok {
			t.Errorf("the guide declares a type %q the SDK does not have", name)
			continue
		}
		for field, typ := range fields {
			got, ok := actual[field]
			if !ok {
				t.Errorf("%s.%s is in the guide and not in the SDK", name, field)
				continue
			}
			checked++
			if got != typ {
				t.Errorf("%s.%s is %s in the guide and %s in the SDK; an author who "+
					"believes the guide gets a compile error, or worse a conversion "+
					"that happens to work", name, field, typ, got)
			}
		}
	}
	t.Logf("checked %d field(s) across %d struct(s) shown in the guide", checked, len(shown))
}

// sdkStructFields maps each exported SDK struct to field name -> type text.
func sdkStructFields(t *testing.T) map[string]map[string]string {
	t.Helper()

	out := map[string]map[string]string{}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, filepath.Join("..", "sdk", "plugin"),
		func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatalf("parsing the SDK: %v", err)
	}

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok || !ts.Name.IsExported() {
					return true
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					return true
				}
				fields := map[string]string{}
				for _, f := range st.Fields.List {
					typ := typeText(fset, f.Type)
					for _, id := range f.Names {
						fields[id.Name] = typ
					}
				}
				out[ts.Name.Name] = fields
				return true
			})
		}
	}
	return out
}

// structsInGoBlocks parses `type X struct { ... }` out of the guide's Go fences.
func structsInGoBlocks(t *testing.T, guide string) map[string]map[string]string {
	t.Helper()

	out := map[string]map[string]string{}
	inGo := false
	var block []string
	flush := func() {
		if len(block) == 0 {
			return
		}
		src := "package p\n" + strings.Join(block, "\n")
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "guide.go", src, 0)
		if err != nil {
			// Most blocks are fragments and will not parse. Only complete
			// declarations are checkable, which is the point.
			block = nil
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			fields := map[string]string{}
			for _, f := range st.Fields.List {
				typ := typeText(fset, f.Type)
				for _, id := range f.Names {
					fields[id.Name] = typ
				}
			}
			out[ts.Name.Name] = fields
			return true
		})
		block = nil
	}

	for _, line := range strings.Split(guide, "\n") {
		if strings.HasPrefix(line, "```go") {
			inGo, block = true, nil
			continue
		}
		if strings.HasPrefix(line, "```") {
			if inGo {
				flush()
			}
			inGo = false
			continue
		}
		if inGo {
			block = append(block, line)
		}
	}
	return out
}

// typeText renders a type expression back to source.
func typeText(fset *token.FileSet, expr ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, expr); err != nil {
		return "?"
	}
	return b.String()
}

// The guide's own manifest has to be one Core would accept.
//
// Nothing checked this. The sample carried max_body_bytes alongside
// needs_request_body: false — which Core now rejects, because the cap is read
// only where the request body is attached and means nothing otherwise. The
// guide would have been handing every new plugin author a manifest that fails
// validation on first run, and the only signal would have been their error,
// not ours.
//
// The same shape as TestGuideCompiles and TestGuideStructsMatchTheSDK: the
// guide is executable and gets run.
func TestGuideManifestsValidate(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "docs", "plugin-development.md"))
	if err != nil {
		t.Fatalf("reading the guide: %v", err)
	}

	blocks := yamlBlocks(string(guide))
	if len(blocks) == 0 {
		t.Fatal("the guide shows no yaml blocks; this test used to check something")
	}

	checked := 0
	for i, block := range blocks {
		// Only whole manifests. The guide also shows fragments — a filters:
		// list on its own, a config: list on its own — which are illustrations
		// of one field rather than files anybody saves.
		if !strings.Contains(block, "key:") || !strings.Contains(block, "version:") {
			continue
		}
		checked++

		// Through Load rather than a decoder of our own, so the block meets
		// everything a real package meets — including the unknown-field
		// rejection, which a hand-rolled parse would skip.
		path := filepath.Join(t.TempDir(), "manifest.yaml")
		if err := os.WriteFile(path, []byte(block), 0o600); err != nil {
			t.Fatalf("writing the block: %v", err)
		}
		m, err := manifest.Load(path)
		if err != nil {
			t.Errorf("yaml block %d is not a manifest Core would load: %v\n%s", i, err, block)
			continue
		}
		if err := m.Validate(); err != nil {
			t.Errorf("yaml block %d is a manifest Core would refuse: %v\n%s", i, err, block)
		}
	}
	if checked == 0 {
		t.Fatal("no complete manifest found in the guide; either the guide stopped " +
			"showing one or the heuristic for finding it broke")
	}
	t.Logf("%d complete manifest(s) in the guide, all accepted by Validate", checked)
}

// yamlBlocks returns the contents of every ```yaml fence.
func yamlBlocks(doc string) []string {
	var out []string
	for rest := doc; ; {
		start := strings.Index(rest, "```yaml")
		if start < 0 {
			return out
		}
		rest = rest[start+len("```yaml"):]
		end := strings.Index(rest, "```")
		if end < 0 {
			return out
		}
		out = append(out, strings.TrimPrefix(rest[:end], "\n"))
		rest = rest[end+3:]
	}
}
