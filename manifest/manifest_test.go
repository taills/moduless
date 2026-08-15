package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func TestLoadParsesMenuTree(t *testing.T) {
	path := writeManifest(t, `
key: reports
display_name: 报表
version: 1.0.0
runtime:
  entrypoint: bin/plugin
menus:
  - path: /reports
    title: 报表
    icon: chart
    order: 10
    children:
      - path: /reports/daily
        title: 日报
        entry: /plugins/reports/daily.html
      - path: /reports/monthly
        title: 月报
        roles: [admin]
`)

	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.Menus) != 1 {
		t.Fatalf("parsed %d root nodes, want 1", len(m.Menus))
	}
	root := m.Menus[0]
	if root.Path != "/reports" || root.Order != 10 {
		t.Errorf("root = %+v", root)
	}
	if len(root.Children) != 2 {
		t.Fatalf("root has %d children, want 2", len(root.Children))
	}
	if root.Children[0].Entry != "/plugins/reports/daily.html" {
		t.Errorf("explicit entry was lost: %+v", root.Children[0])
	}
	if len(root.Children[1].Roles) != 1 || root.Children[1].Roles[0] != "admin" {
		t.Errorf("roles were lost: %+v", root.Children[1])
	}
}

func TestLoadRejectsBadMenus(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "node without a path",
			body: "key: a\nversion: 1\nmenus:\n  - title: 无路径\n",
		},
		{
			// Within one plugin a repeated path is a mistake. Across plugins it
			// is how a shared parent is expressed, which is why the check is
			// per-plugin.
			name: "duplicate path in the same plugin",
			body: "key: a\nversion: 1\nmenus:\n  - path: /x\n    title: 一\n  - path: /x\n    title: 二\n",
		},
		{
			name: "duplicate path nested",
			body: "key: a\nversion: 1\nmenus:\n  - path: /x\n    title: 一\n    children:\n      - path: /x\n        title: 二\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeManifest(t, tc.body)); err == nil {
				t.Error("Load accepted an invalid menu tree")
			}
		})
	}
}

func TestLoadReportsMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("Load succeeded on a missing file")
	}
}

func TestLoadReportsMalformedYAML(t *testing.T) {
	if _, err := Load(writeManifest(t, "key: [unclosed\n")); err == nil {
		t.Error("Load accepted malformed YAML")
	}
}

func TestConfigDeclarationValidation(t *testing.T) {
	base := func(cfg []ConfigDecl) *Manifest {
		return &Manifest{
			Key:     "p",
			Version: "1.0.0",
			Runtime: Runtime{Entrypoint: "bin/p"},
			Config:  cfg,
		}
	}

	tests := []struct {
		name    string
		config  []ConfigDecl
		wantErr bool
	}{
		{"empty is fine", nil, false},
		{"a plain key", []ConfigDecl{{Key: "retention_days"}}, false},
		{"known types", []ConfigDecl{
			{Key: "a", Type: "string"}, {Key: "b", Type: "int"},
			{Key: "c", Type: "bool"}, {Key: "d", Type: "duration"},
			{Key: "e", Type: "text"},
		}, false},
		{"missing key", []ConfigDecl{{Label: "no key"}}, true},
		{"duplicate key", []ConfigDecl{{Key: "x"}, {Key: "x"}}, true},
		{"unknown type", []ConfigDecl{{Key: "x", Type: "json"}}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := base(tc.config).Validate()
			if tc.wantErr && err == nil {
				t.Error("Validate accepted an invalid config declaration")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate rejected a valid one: %v", err)
			}
		})
	}
}

// Declared defaults reach the plugin, so its OnConfigChanged sees a complete
// map rather than having to re-state every default in code.
func TestMergeConfigSuppliesDefaults(t *testing.T) {
	m := &Manifest{Config: []ConfigDecl{
		{Key: "retention_days", Default: "30"},
		{Key: "verbose", Default: "false"},
		{Key: "no_default"},
	}}

	got := m.MergeConfig(map[string]string{"verbose": "true"})

	if got["retention_days"] != "30" {
		t.Errorf("retention_days = %q, want the declared default", got["retention_days"])
	}
	if got["verbose"] != "true" {
		t.Errorf("verbose = %q; an operator's value must win over the default", got["verbose"])
	}
	if _, present := got["no_default"]; present {
		t.Error("a key with no default was invented out of nothing")
	}
}

// Clearing a field is a decision. Re-supplying the default would make an empty
// value impossible to express.
func TestMergeConfigKeepsAnExplicitEmptyValue(t *testing.T) {
	m := &Manifest{Config: []ConfigDecl{{Key: "prefix", Default: "audit-"}}}

	got := m.MergeConfig(map[string]string{"prefix": ""})
	if got["prefix"] != "" {
		t.Errorf("prefix = %q; an operator clearing a field had it filled back in", got["prefix"])
	}
}

// A manifest declaring nothing behaves exactly as before.
func TestMergeConfigWithoutDeclarations(t *testing.T) {
	m := &Manifest{}
	set := map[string]string{"whatever": "1"}
	if got := m.MergeConfig(set); got["whatever"] != "1" || len(got) != 1 {
		t.Errorf("MergeConfig altered an undeclared config: %v", got)
	}
}

// A manifest is a declaration, not a payload. One that is enormous is a
// mistake — a generator that looped, something written to the wrong path — and
// refusing it before parsing beats reading megabytes into memory and building
// an object graph out of them.
func TestLoadRejectsAnEnormousManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")

	var b strings.Builder
	b.WriteString("key: p\nversion: 1.0.0\nruntime:\n  entrypoint: bin/p\nmenus:\n")
	for i := 0; b.Len() < MaxManifestBytes+1024; i++ {
		fmt.Fprintf(&b, "  - path: /n%d\n    title: %s\n", i, strings.Repeat("x", 200))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Errorf("a %d byte manifest was accepted", b.Len())
	} else {
		t.Logf("refused: %v", err)
	}
}
