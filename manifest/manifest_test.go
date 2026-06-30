package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
