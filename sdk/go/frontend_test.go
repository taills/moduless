package sdk

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildFrontendZip(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.html"), "<html>hi</html>")
	writeFile(t, filepath.Join(dir, "assets", "app.js"), "console.log(1)")

	data, sum, err := buildFrontendZip(dir)
	if err != nil {
		t.Fatalf("buildFrontendZip: %v", err)
	}

	// SHA-256 must match the returned bytes so Core can verify the upload.
	want := sha256.Sum256(data)
	if hex.EncodeToString(want[:]) != sum {
		t.Fatalf("sha256 mismatch: got %s", sum)
	}

	// Entry names must be slash-separated and relative to dir, matching the
	// keys Core derives ("/"+name) and the gateway serves.
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	got := map[string]string{}
	for _, f := range r.File {
		rc, _ := f.Open()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(rc)
		rc.Close()
		got[f.Name] = buf.String()
	}
	if got["index.html"] != "<html>hi</html>" {
		t.Errorf("index.html content = %q", got["index.html"])
	}
	if got["assets/app.js"] != "console.log(1)" {
		t.Errorf("assets/app.js content = %q", got["assets/app.js"])
	}
}

func TestBuildFrontendZipMissingDir(t *testing.T) {
	if _, _, err := buildFrontendZip(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing dir")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
