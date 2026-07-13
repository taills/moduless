package gateway

import (
	"reflect"
	"testing"
)

// TestRoleAllowed verifies the role filter rules. Empty Roles = open to
// anyone; non-empty Roles = strictly allowlist (empty userRole denied).
func TestRoleAllowed(t *testing.T) {
	cases := []struct {
		name     string
		roles    []string
		userRole string
		want     bool
	}{
		{"empty roles: anyone", nil, "admin", true},
		{"empty roles: no session still OK", nil, "", true},
		{"admin role allowed", []string{"admin"}, "admin", true},
		{"user role denied by admin-only node", []string{"admin"}, "user", false},
		{"no session denied by any roles", []string{"admin", "user"}, "", false},
		{"multi-role: match one", []string{"admin", "ops"}, "ops", true},
		{"multi-role: no match", []string{"admin", "ops"}, "user", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := roleAllowed(tc.roles, tc.userRole); got != tc.want {
				t.Fatalf("roleAllowed(%v, %q) = %v, want %v", tc.roles, tc.userRole, got, tc.want)
			}
		})
	}
}

// TestBuildTree covers the cross-extension merge + role filter rules.
func TestBuildTree(t *testing.T) {
	extA := []MenuNode{
		{Path: "/system", Title: "System", Order: 0, Children: []MenuNode{
			{Path: "/system/a-config", Title: "A Config", Order: 0, Entry: "/extensions/a/"},
		}},
	}
	extB := []MenuNode{
		{Path: "/system", Title: "System Override (ignored)", Order: 0, Children: []MenuNode{
			{Path: "/system/b-config", Title: "B Config", Order: 1, Entry: "/extensions/b/"},
		}},
		{Path: "/admin", Title: "Admin", Order: 1, Roles: []string{"admin"}},
	}

	t.Run("admin sees admin-only node and merged /system", func(t *testing.T) {
		got := buildTree([][]MenuNode{extA, extB}, "admin")
		if len(got) != 2 {
			t.Fatalf("want 2 roots, got %d: %+v", len(got), got)
		}
		// sort by Order then Path → /admin(1) before /system(0)? No: Order is per
		// sibling group. Here both are roots. After sort by (Order,Path):
		// /admin (Order=1) > /system (Order=0). So /system comes first.
		if got[0].Path != "/system" {
			t.Fatalf("first root should be /system, got %s", got[0].Path)
		}
		if got[1].Path != "/admin" {
			t.Fatalf("second root should be /admin, got %s", got[1].Path)
		}
		// /system has 2 children after merge, sorted by Order: a-config(0) then b-config(1).
		if len(got[0].Children) != 2 || got[0].Children[0].Path != "/system/a-config" || got[0].Children[1].Path != "/system/b-config" {
			t.Fatalf("merged /system children wrong: %+v", got[0].Children)
		}
		// First writer wins title: extA declared "/system" first.
		if got[0].Title != "System" {
			t.Fatalf("first-writer should win title: got %s", got[0].Title)
		}
	})

	t.Run("non-admin does not see admin-only node", func(t *testing.T) {
		got := buildTree([][]MenuNode{extA, extB}, "user")
		if len(got) != 1 || got[0].Path != "/system" {
			t.Fatalf("want only /system for user, got %+v", got)
		}
	})

	t.Run("empty menus yields empty tree", func(t *testing.T) {
		got := buildTree(nil, "admin")
		if len(got) != 0 {
			t.Fatalf("want empty, got %+v", got)
		}
	})
}

// TestFilterByRoleRecursive makes sure role filtering drops hidden subtrees
// rather than leaving empty containers.
func TestFilterByRoleRecursive(t *testing.T) {
	nodes := []MenuNode{
		{Path: "/system", Title: "S", Children: []MenuNode{
			{Path: "/system/admin-only", Title: "AO", Roles: []string{"admin"}},
			{Path: "/system/public", Title: "Pub"},
		}},
		{Path: "/hidden", Title: "H", Roles: []string{"admin"}},
	}
	got := filterByRole(nodes, "user")
	if len(got) != 1 || got[0].Path != "/system" {
		t.Fatalf("only /system should remain for user: %+v", got)
	}
	if len(got[0].Children) != 1 || got[0].Children[0].Path != "/system/public" {
		t.Fatalf("admin-only child should be filtered out: %+v", got[0].Children)
	}
}

// TestSortMenuNodes makes sure sibling ordering is stable across runs.
func TestSortMenuNodes(t *testing.T) {
	in := []MenuNode{
		{Path: "/c", Order: 2},
		{Path: "/a", Order: 0},
		{Path: "/b", Order: 1},
		{Path: "/z", Order: 1}, // same order as /b → tie broken by path
	}
	sortMenuNodes(in)
	want := []string{"/a", "/b", "/z", "/c"}
	got := make([]string, 0, len(in))
	for _, n := range in {
		got = append(got, n.Path)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sort: want %v, got %v", want, got)
	}
}
