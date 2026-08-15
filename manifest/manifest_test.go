package manifest

import (
	"os"
	"path/filepath"
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
