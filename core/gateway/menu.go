package gateway

import (
	"sort"
)

// MenuNode is one node in the host app menu tree. Field semantics mirror
// manifest.MenuItem / proto.MenuItem; it is the public type Core returns to
// the host (it has json tags the frontend reads).
type MenuNode struct {
	Path     string     `json:"path"`
	Title    string     `json:"title"`
	Icon     string     `json:"icon,omitempty"`
	Order    int        `json:"order"`
	Entry    string     `json:"entry,omitempty"`
	Roles    []string   `json:"roles,omitempty"`
	Children []MenuNode `json:"children,omitempty"`
}

// roleAllowed reports whether a node with the given roles list is visible to a
// user with role userRole. An empty Roles list means "any role, including
// unauthenticated callers"; an empty userRole (no session) only sees nodes
// whose Roles is also empty. This intentionally errs on the side of denying
// when the data is unclear.
func roleAllowed(roles []string, userRole string) bool {
	if len(roles) == 0 {
		return true
	}
	if userRole == "" {
		return false
	}
	for _, r := range roles {
		if r == userRole {
			return true
		}
	}
	return false
}

// buildTree merges menu nodes from many extensions into a single tree.
//
//   - Nodes are deduplicated by Path: same Path → same logical node, with
//     children merged recursively. The first extension in `menus` to declare a
//     given path wins its title/icon (later declarations of the same path are
//     ignored for those fields — useful when multiple extensions co-author a
//     shared parent like "/system").
//   - Roles filtering happens before merge: nodes whose Roles exclude the
//     current userRole are dropped entirely (they don't appear in the tree,
//     not even as empty containers).
//   - Siblings are sorted ascending by Order, then by Path for stability.
func buildTree(menus [][]MenuNode, userRole string) []MenuNode {
	var root []MenuNode
	for _, appMenus := range menus {
		filtered := filterByRole(appMenus, userRole)
		for i := range filtered {
			mergeInto(&root, &filtered[i])
		}
	}
	sortMenuNodes(root)
	return root
}

// filterByRole returns the subset of nodes (recursively) the userRole can see.
func filterByRole(nodes []MenuNode, userRole string) []MenuNode {
	out := make([]MenuNode, 0, len(nodes))
	for _, n := range nodes {
		if !roleAllowed(n.Roles, userRole) {
			continue
		}
		n.Children = filterByRole(n.Children, userRole)
		out = append(out, n)
	}
	return out
}

// mergeInto folds node into the siblings slice, appending a new node when no
// sibling shares its Path, or recursively merging into the existing one.
// Children are sorted after the merge so the JSON output is deterministic.
func mergeInto(siblings *[]MenuNode, node *MenuNode) {
	idx := findIndex(*siblings, node.Path)
	if idx < 0 {
		// First writer of this Path: clone without children (children will be
		// merged in below).
		copy := *node
		copy.Children = nil
		*siblings = append(*siblings, copy)
		idx = len(*siblings) - 1
	}
	for i := range node.Children {
		child := node.Children[i]
		mergeInto(&(*siblings)[idx].Children, &child)
	}
	sortMenuNodes((*siblings)[idx].Children)
}

// findIndex returns the index of the first node with the given Path, or -1.
func findIndex(nodes []MenuNode, path string) int {
	for i := range nodes {
		if nodes[i].Path == path {
			return i
		}
	}
	return -1
}

// sortMenuNodes orders nodes ascending by Order then by Path for stability.
// Used after both filter and merge so the JSON output is deterministic.
func sortMenuNodes(nodes []MenuNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Order != nodes[j].Order {
			return nodes[i].Order < nodes[j].Order
		}
		return nodes[i].Path < nodes[j].Path
	})
}
