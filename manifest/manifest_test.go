package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMenuNormalization exercises the legacy→new menu upgrade and validation
// rules in normalizeMenus.
func TestMenuNormalization(t *testing.T) {
	cases := []struct {
		name       string
		yaml       string
		wantMenus  int      // expected top-level Menus count
		wantPaths  []string // paths that must appear (any depth)
		wantFirst  string   // path of the first top-level node
		wantErrSub string   // expected substring in error, "" if success
	}{
		{
			name: "legacy single menu promoted",
			yaml: `key: k
display_name: K
menu:
  icon: gear
  path: /system
`,
			wantMenus: 1,
			wantPaths: []string{"/system"},
			wantFirst: "/system",
		},
		{
			name: "explicit menus kept as-is",
			yaml: `key: k
display_name: K
menus:
  - path: /system
    title: System
    icon: gear
    order: 0
    children:
      - path: /system/a
        title: A
        order: 0
        entry: /extensions/k_a/index.html
`,
			wantMenus: 1,
			wantPaths: []string{"/system", "/system/a"},
			wantFirst: "/system",
		},
		{
			name: "explicit menus wins over legacy menu",
			yaml: `key: k
display_name: K
menu:
  path: /legacy
menus:
  - path: /new
    title: New
`,
			wantMenus: 1,
			wantPaths: []string{"/new"},
			wantFirst: "/new",
		},
		{
			name: "no menus at all leaves Menus empty",
			yaml: `key: k
display_name: K
`,
			wantMenus: 0,
		},
		{
			name: "missing path is rejected",
			yaml: `key: k
menus:
  - title: NoPath
`,
			wantErrSub: "missing path",
		},
		{
			name: "duplicate path is rejected",
			yaml: `key: k
menus:
  - path: /dup
  - path: /dup
`,
			wantErrSub: "duplicate menu path",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "manifest.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			m, err := Load(path)
			if tc.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrSub)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(m.Menus) != tc.wantMenus {
				t.Fatalf("menus count: want %d, got %d (menus=%+v)", tc.wantMenus, len(m.Menus), m.Menus)
			}
			if tc.wantFirst != "" && len(m.Menus) > 0 && m.Menus[0].Path != tc.wantFirst {
				t.Fatalf("first menu path: want %q, got %q", tc.wantFirst, m.Menus[0].Path)
			}
			for _, want := range tc.wantPaths {
				if !containsPath(m.Menus, want) {
					t.Fatalf("expected path %q in menus, got %+v", want, m.Menus)
				}
			}
		})
	}
}

func containsPath(nodes []MenuItem, target string) bool {
	for _, n := range nodes {
		if n.Path == target {
			return true
		}
		if containsPath(n.Children, target) {
			return true
		}
	}
	return false
}

func TestSaveSecretAppendsAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	original := "key: demo\n# a comment\nversion: \"1.0.0\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// First save appends the secret while preserving existing lines/comments.
	if err := SaveSecret(path, "ext_abc"); err != nil {
		t.Fatalf("save: %v", err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Secret != "ext_abc" || m.Key != "demo" || m.Version != "1.0.0" {
		t.Fatalf("unexpected manifest after first save: %+v", m)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# a comment") {
		t.Fatalf("comment not preserved: %s", data)
	}

	// Second save replaces the existing secret line rather than duplicating it.
	if err := SaveSecret(path, "ext_xyz"); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	data, _ = os.ReadFile(path)
	if n := strings.Count(string(data), "secret:"); n != 1 {
		t.Fatalf("expected exactly one secret line, got %d:\n%s", n, data)
	}
	m, _ = Load(path)
	if m.Secret != "ext_xyz" {
		t.Fatalf("secret not replaced: %q", m.Secret)
	}
}
