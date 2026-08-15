package pluginhost

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/taills/moduless/manifest"
)

// ManifestFilename is the declaration file every plugin package carries.
const ManifestFilename = "manifest.yaml"

// Package is an installed plugin package on disk, already parsed and
// validated. It is not running: enabling it is a separate, explicit step.
type Package struct {
	// Dir is the package root.
	Dir string

	// Manifest is the parsed, validated declaration.
	Manifest *manifest.Manifest

	// BinaryPath is the absolute path to the executable.
	BinaryPath string

	// Checksum is the SHA-256 of BinaryPath as it was when this package was
	// loaded and validated. go-plugin re-verifies it immediately before exec,
	// so the bytes that run are the bytes that were checked.
	Checksum []byte

	// FrontendDir is the built micro-frontend, or empty when the plugin has
	// no UI. Unlike the reverse-tunnel model, these files live on disk and
	// survive a Core restart instead of being re-uploaded over a connection.
	FrontendDir string

	// Filters are the manifest's compiled lifecycle subscriptions.
	Filters []manifest.CompiledFilter
}

// Key is the plugin's identifier.
func (p *Package) Key() string { return p.Manifest.Key }

// Version is the package version.
func (p *Package) Version() string { return p.Manifest.Version }

// AllowsIdentityMutation reports whether this plugin may set request identity
// from an authentication filter.
func (p *Package) AllowsIdentityMutation() bool {
	return p.Manifest.HasPermission(manifest.PermFilterAuthenticate)
}

// LoadPackage reads and validates one plugin package directory.
//
// Validation is strict and happens here rather than at launch, so a
// misconfigured plugin is rejected while an admin is looking at the result
// instead of failing later inside a request.
func LoadPackage(dir string) (*Package, error) {
	manifestPath := filepath.Join(dir, ManifestFilename)
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", manifestPath, err)
	}

	entrypoint := m.Runtime.Entrypoint
	if entrypoint == "" {
		return nil, fmt.Errorf("%s: runtime.entrypoint is required", manifestPath)
	}
	binary := filepath.Join(dir, filepath.Clean("/"+entrypoint))
	info, err := os.Stat(binary)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: entrypoint %s: %w", m.Key, entrypoint, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("plugin %s: entrypoint %s is a directory", m.Key, entrypoint)
	}
	if info.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("plugin %s: entrypoint %s is not executable", m.Key, entrypoint)
	}

	filters, err := m.CompileFilters()
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", m.Key, err)
	}

	// Hash the bytes we just validated, so what executes is what was checked.
	// go-plugin verifies this again immediately before exec, which closes the
	// window between a package being inspected and its binary being run — the
	// case where something replaces the file in between.
	sum, err := fileChecksum(binary)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", m.Key, err)
	}

	pkg := &Package{
		Dir:        dir,
		Manifest:   m,
		BinaryPath: binary,
		Checksum:   sum,
		Filters:    filters,
	}
	if fe := filepath.Join(dir, "frontend"); dirExists(fe) {
		pkg.FrontendDir = fe
	}
	return pkg, nil
}

// ScanPackages loads every plugin package directly under root.
//
// A package that fails to load does not abort the scan: one bad plugin must
// not stop Core from starting the others. Failures are returned alongside the
// successes so the caller can surface them.
func ScanPackages(root string) (packages []*Package, failures map[string]error) {
	failures = map[string]error{}

	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			failures["*"] = fmt.Errorf("read plugin directory %s: %w", root, err)
		}
		return nil, failures
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())

		// A directory with no manifest is not a failed plugin, it is not a
		// plugin. Reporting it as broken fills the console with things an
		// operator cannot act on — the per-plugin data directory when
		// PLUGIN_DATA_DIR sits inside PLUGIN_DIR, an unpacking leftover, a
		// version-control directory. A directory that does have a manifest is
		// claiming to be a plugin, and anything wrong with it from there on is
		// a genuine failure worth showing.
		if _, err := os.Stat(filepath.Join(dir, ManifestFilename)); err != nil {
			continue
		}

		pkg, err := LoadPackage(dir)
		if err != nil {
			failures[e.Name()] = err
			continue
		}
		if pkg.Key() != e.Name() {
			// The directory name is what the admin sees and what the
			// content-addressed layout keys on; a mismatch is a packaging
			// error worth surfacing rather than silently trusting.
			failures[e.Name()] = fmt.Errorf(
				"plugin key %q does not match its directory name %q", pkg.Key(), e.Name())
			continue
		}
		packages = append(packages, pkg)
	}

	sort.Slice(packages, func(i, j int) bool { return packages[i].Key() < packages[j].Key() })
	return packages, failures
}

// fileChecksum is the SHA-256 of a file's contents.
func fileChecksum(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return h.Sum(nil), nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
