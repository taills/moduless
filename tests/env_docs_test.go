package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The environment Core reads and the environment the docs promise are one set.
//
// Both directions fail quietly. A variable Core reads and nobody documents is
// a knob only its author knows about — the operator who needs it never finds
// it. A variable the docs promise and Core no longer reads is worse: somebody
// sets it, deploys, and the behaviour they were told to expect does not
// happen, with nothing anywhere to say why.
//
// That second one is not hypothetical here. PLUGIN_DEV_MODE was documented in
// three places and read in one, and when it was removed the entries had to be
// hunted down by hand.

var envReference = regexp.MustCompile(`(?:env|os\.Getenv)\("([A-Z][A-Z0-9_]{2,})"`)

// docEnvNames are variables named in the documentation tables. Written as a
// literal rather than scraped, because scraping prose finds words like COPY in
// a Dockerfile snippet and calls them configuration.
var documented = []string{
	"ADMIN_PASSWORD",
	"ADMIN_USERNAME",
	"DATABASE_URL",
	"HOST_FRONTEND_DIR",
	"HTTP_ADDR",
	"PLUGIN_DATA_DIR",
	"PLUGIN_DIR",
	"PLUGIN_LOG_LEVEL",
	"RUSTFS_ACCESS_KEY",
	"RUSTFS_BUCKET",
	"RUSTFS_ENDPOINT",
	"RUSTFS_SECRET_KEY",
}

func TestDocumentedEnvironmentMatchesWhatCoreReads(t *testing.T) {
	read := map[string]string{} // name -> file that reads it

	root := filepath.Join("..", "core")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range envReference.FindAllSubmatch(src, -1) {
			read[string(m[1])] = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking core: %v", err)
	}
	if len(read) == 0 {
		t.Fatal("found no environment variables at all; the pattern stopped matching " +
			"and this test now passes for the wrong reason")
	}

	for name, file := range read {
		if !slices.Contains(documented, name) {
			t.Errorf("%s is read by %s and appears in no documentation table. A knob "+
				"only its author knows about is one the operator who needs it will "+
				"not find", name, file)
		}
	}
	for _, name := range documented {
		if _, ok := read[name]; !ok {
			t.Errorf("%s is documented and nothing in core reads it. Somebody sets it, "+
				"deploys, and the behaviour they were promised does not happen — with "+
				"nothing to say why", name)
		}
	}
}
