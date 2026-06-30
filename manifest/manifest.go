// Package manifest parses an extension's declarative manifest.yaml describing
// its database collections, indexes and UI slots. Both the SDK (to send
// declarations on registration) and Core (to reconcile schema) use it.
package manifest

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Manifest is the root of an extension manifest.yaml.
type Manifest struct {
	Key         string   `yaml:"key"`
	DisplayName string   `yaml:"display_name"`
	Version     string   `yaml:"version"`
	Menu        Menu     `yaml:"menu"`
	Frontend    Frontend `yaml:"frontend"`
	Database    Database `yaml:"database"`
	UISlots     []Slot   `yaml:"ui_slots"`
}

type Menu struct {
	Icon string `yaml:"icon"`
	Path string `yaml:"path"`
}

type Frontend struct {
	Entry string `yaml:"entry"`
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

type Slot struct {
	SlotName       string `yaml:"slot_name"`
	ComponentEntry string `yaml:"component_entry"`
}

// Load reads and parses a manifest.yaml from disk.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	return &m, nil
}
