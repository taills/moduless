// Package manifest parses a plugin's declarative manifest.yaml.
//
// Core reads it before starting the plugin process, so everything it declares
// — collections, menus, filters, jobs, permissions — is known and validated
// while an admin is looking at the result, rather than failing later inside a
// request.
package manifest

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Manifest is the root of a plugin's manifest.yaml.
type Manifest struct {
	Key         string `yaml:"key"`
	DisplayName string `yaml:"display_name"`
	Version     string `yaml:"version"`

	// Weight biases smooth weighted round-robin across a plugin's replicas.
	// Zero means 1.
	Weight int `yaml:"weight"`

	// Runtime tells Core how to launch the plugin process.
	Runtime Runtime `yaml:"runtime"`

	// Permissions lists the Host capabilities the plugin requests.
	//
	// Core enforces this on its own side of the connection, so a plugin cannot
	// reach a capability it did not declare. Its main value, though, is that it
	// is the review checklist: an operator approving a plugin can see at a
	// glance that it wants the queue and outbound HTTP but not files.
	Permissions []string `yaml:"permissions"`

	// Menus is the plugin's menu tree in the console. It appears when the
	// plugin is enabled and disappears when it is disabled.
	Menus []MenuItem `yaml:"menus"`

	// Database declares the collections Core provisions before the plugin runs.
	Database Database `yaml:"database"`

	// Filters subscribes the plugin to request lifecycle phases.
	Filters []FilterDecl `yaml:"filters"`

	// Jobs are cron schedules Core runs on the plugin's behalf.
	Jobs []JobDecl `yaml:"jobs"`

	// EgressAllow is the hostname allow-list for outbound HTTP made through
	// Core's proxy. Plugins have no direct network access.
	EgressAllow []string `yaml:"egress_allow"`
}

// MenuItem is one node in a plugin's menu tree.
//
// An empty Entry on a leaf means "mount this plugin's own micro-frontend"; a
// node with children is organisational and stays entry-less so the console
// does not try to mount it. Roles, when non-empty, restricts which user roles
// see the node — Core filters before the tree reaches the browser.
type MenuItem struct {
	Path     string     `yaml:"path"`
	Title    string     `yaml:"title"`
	Icon     string     `yaml:"icon"`
	Order    int        `yaml:"order"`
	Entry    string     `yaml:"entry"`
	Roles    []string   `yaml:"roles"`
	Children []MenuItem `yaml:"children"`
}

type Database struct {
	Collections []Collection `yaml:"collections"`
}

type Collection struct {
	Name    string  `yaml:"name"`
	Indexes []Index `yaml:"indexes"`
}

type Index struct {
	Fields []string `yaml:"fields"`
	Unique bool     `yaml:"unique"`
}

// Load reads and parses a manifest.yaml, then checks its menu tree.
//
// Call Validate afterwards for the full check; Load only covers what parsing
// itself can establish.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if err := validateMenus(m.Menus); err != nil {
		return nil, fmt.Errorf("menus in %s: %w", path, err)
	}
	return &m, nil
}

// validateMenus checks that every node has a path and that no path repeats
// within one plugin.
//
// Paths colliding *across* plugins is expected and meaningful — that is how
// two plugins share a parent like "/reports" — so this check is deliberately
// per-plugin only.
func validateMenus(nodes []MenuItem) error {
	seen := make(map[string]struct{})

	var walk func([]MenuItem) error
	walk = func(nodes []MenuItem) error {
		for _, n := range nodes {
			if n.Path == "" {
				return fmt.Errorf("menu node %q is missing a path", n.Title)
			}
			if _, dup := seen[n.Path]; dup {
				return fmt.Errorf("duplicate menu path %q", n.Path)
			}
			seen[n.Path] = struct{}{}
			if err := walk(n.Children); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(nodes)
}
