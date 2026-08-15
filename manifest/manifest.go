// Package manifest parses a plugin's declarative manifest.yaml.
//
// Core reads it before starting the plugin process, so everything it declares
// — collections, menus, filters, jobs, permissions — is known and validated
// while an admin is looking at the result, rather than failing later inside a
// request.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
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

	// Config declares the settings an operator can fill in for this plugin.
	//
	// Without it, the key names a plugin reads are an unwritten agreement
	// between whoever wrote the code and whoever fills in the console: a typo
	// on either side produces a plugin silently running on its compiled-in
	// defaults, and there is no moment at which anything could notice. The
	// declaration gives the console something to render, gives Core the
	// defaults to supply, and gives a reviewer a list of what the plugin is
	// configurable to do.
	Config []ConfigDecl `yaml:"config"`
}

// ConfigDecl is one setting a plugin accepts.
type ConfigDecl struct {
	Key string `yaml:"key"`

	// Label and Description are what the console shows. Label falls back to
	// the key.
	Label       string `yaml:"label"`
	Description string `yaml:"description"`

	// Type drives the console's input and nothing else: values always reach
	// the plugin as strings, because a manifest cannot be trusted to describe
	// what an operator actually typed.
	Type string `yaml:"type"`

	// Default is supplied to the plugin when an operator has set no value.
	Default string `yaml:"default"`

	// Required marks a setting the plugin cannot run without. Core does not
	// refuse to start the plugin over it — that would make a missing value an
	// outage — but the console can mark it and an operator can see it.
	Required bool `yaml:"required"`

	// Secret hides the value in the console and in any dump of the config.
	Secret bool `yaml:"secret"`
}

// ConfigTypes are the input kinds the console knows how to render.
var ConfigTypes = map[string]struct{}{
	"string": {}, "int": {}, "bool": {}, "duration": {}, "text": {},
}

// ConfigDefaults returns the declared defaults, keyed by setting.
func (m *Manifest) ConfigDefaults() map[string]string {
	if len(m.Config) == 0 {
		return nil
	}
	out := make(map[string]string, len(m.Config))
	for _, c := range m.Config {
		if c.Default != "" {
			out[c.Key] = c.Default
		}
	}
	return out
}

// MergeConfig fills in declared defaults for anything an operator left unset,
// so a plugin's OnConfigChanged always receives a complete map.
//
// Values an operator did set win, including empty ones: clearing a field is a
// decision, and re-supplying the default would make it impossible to express.
func (m *Manifest) MergeConfig(set map[string]string) map[string]string {
	defaults := m.ConfigDefaults()
	if len(defaults) == 0 {
		return set
	}
	out := make(map[string]string, len(defaults)+len(set))
	maps.Copy(out, defaults)
	maps.Copy(out, set)
	return out
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
// MaxManifestBytes bounds the declaration file.
//
// A real manifest is a few kilobytes. This is generous enough that no honest
// one approaches it, and low enough that a file which does — a generator that
// looped, a log accidentally written to the wrong path — is refused before it
// is parsed rather than read into memory and turned into an object graph.
const MaxManifestBytes = 1 << 20

func Load(path string) (*Manifest, error) {
	if info, err := os.Stat(path); err == nil && info.Size() > MaxManifestBytes {
		return nil, fmt.Errorf("manifest %s is %d bytes, over the %d byte limit; "+
			"a manifest is a declaration, not data", path, info.Size(), MaxManifestBytes)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var m Manifest
	// Unknown fields are an error, not something to ignore.
	//
	// A manifest is what a reviewer reads to decide whether to install a
	// plugin, and what Core enforces. Silently dropping a field it does not
	// recognise makes those two things differ: `filter:` instead of `filters:`
	// installs cleanly, shows as running, and the plugin's fail-closed
	// authenticate filter never runs — so every request goes unauthenticated
	// while the manifest on screen says otherwise. Most typos happen to fail
	// closed, but that one does not, and it is the one that matters.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil && !errors.Is(err, io.EOF) {
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
