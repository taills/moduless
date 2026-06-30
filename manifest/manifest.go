// Package manifest parses an extension's declarative manifest.yaml describing
// its database collections, indexes and UI slots. Both the SDK (to send
// declarations on registration) and Core (to reconcile schema) use it.
package manifest

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest is the root of an extension manifest.yaml.
type Manifest struct {
	Key         string   `yaml:"key"`
	DisplayName string   `yaml:"display_name"`
	Version     string   `yaml:"version"`
	Weight      int      `yaml:"weight"` // load-balancing weight for replicas (default 1)
	Menu        Menu     `yaml:"menu"`
	Frontend    Frontend `yaml:"frontend"`
	Database    Database `yaml:"database"`
	UISlots     []Slot   `yaml:"ui_slots"`
	// Secret is the credential Core issues when an admin approves this extension.
	// The SDK persists it here (see SaveSecret) and replays it on every reconnect.
	Secret string `yaml:"secret"`
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

// SaveSecret persists the Core-issued secret back into manifest.yaml. It rewrites
// only the top-level `secret:` line (replacing any existing one, otherwise
// appending), leaving the rest of the file — including comments — untouched.
func SaveSecret(path, secret string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read manifest %s: %w", path, err)
	}

	var out []string
	replaced := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// Match a top-level (non-indented) secret key.
		if strings.HasPrefix(line, "secret:") {
			out = append(out, fmt.Sprintf("secret: %q", secret))
			replaced = true
			continue
		}
		out = append(out, line)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan manifest %s: %w", path, err)
	}
	if !replaced {
		out = append(out, fmt.Sprintf("secret: %q", secret))
	}

	content := strings.Join(out, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write manifest %s: %w", path, err)
	}
	return nil
}
