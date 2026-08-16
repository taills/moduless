package tests

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/manifest"
)

// The limits table in the plugin guide had rotted.
//
// MaxOpenTxPerPlugin was raised from 4 to 8 — the reason is still in the
// comment on it — and the table kept saying 4. Nothing failed, because nothing
// compares them. A plugin author sizing their concurrency against the guide got
// half the capacity they were entitled to, and one debugging a refusal would
// have looked for it four transactions too early.
//
// Four of the five rows were right, which is what makes this worth pinning
// rather than re-reading: the table looks maintained, and the one wrong row is
// indistinguishable from the others until someone checks.
//
// The guide names the constant in each row, so this is a lookup rather than a
// guess about which prose number means which limit — and a reader who wants the
// source of truth is told where it is.
func TestDocumentedLimitsMatchTheCode(t *testing.T) {
	limits := map[string]int{
		// Runtime: how much of a shared resource one plugin may hold.
		"db.MaxOpenTxPerPlugin":                    db.MaxOpenTxPerPlugin,
		"hostsvc.DefaultMaxQueueDepth":             hostsvc.DefaultMaxQueueDepth,
		"hostsvc.DefaultMaxLocks":                  hostsvc.DefaultMaxLocks,
		"hostsvc.DefaultMaxSubscriptionsPerPlugin": hostsvc.DefaultMaxSubscriptionsPerPlugin,
		"db.MaxOpenConns":                          db.MaxOpenConns,

		// Declared: what a manifest may ask for. All six were right when this
		// was written; they are here because they rot the same way, not because
		// they were wrong.
		"manifest.MaxManifestBytes": manifest.MaxManifestBytes,
		"manifest.MaxFilters":       manifest.MaxFilters,
		"manifest.MaxCollections":   manifest.MaxCollections,
		"manifest.MaxJobs":          manifest.MaxJobs,
		"manifest.MaxConfigKeys":    manifest.MaxConfigKeys,
		"manifest.MaxMenuDepth":     manifest.MaxMenuDepth,
	}

	raw, err := os.ReadFile("../docs/plugin-development.md")
	if err != nil {
		t.Fatalf("reading the guide: %v", err)
	}

	rows := limitRows(string(raw), limits)

	// A scan that finds nothing reports the same thing as a scan that finds
	// everything in order. Assert the reach before trusting the verdict.
	if len(rows) != len(limits) {
		var missing []string
		for name := range limits {
			if _, ok := rows[name]; !ok {
				missing = append(missing, name)
			}
		}
		t.Fatalf("found %d of %d limits in the guide; no row names %v.\n"+
			"Every runtime limit belongs in that table with its constant, or this "+
			"check silently stops covering it.", len(rows), len(limits), missing)
	}

	for name, want := range limits {
		if got := rows[name]; got != want {
			t.Errorf("the guide says %s is %d, the code says %d", name, got, want)
		}
	}
}

// limitRows pulls "the number on the row that names this constant" out of the
// markdown. Deliberately crude: it wants the first integer in the row, which is
// the limit, and the constant name is what identifies the row.
func limitRows(doc string, limits map[string]int) map[string]int {
	number := regexp.MustCompile(`\d[\d,]*`)

	out := make(map[string]int, len(limits))
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		for name := range limits {
			if !strings.Contains(line, "`"+name+"`") {
				continue
			}
			// Strip the constant itself before looking for the number, or a
			// name ending in a digit would be read as the limit.
			cleaned := strings.ReplaceAll(line, "`"+name+"`", "")
			m := number.FindString(cleaned)
			if m == "" {
				continue
			}
			n, err := strconv.Atoi(strings.ReplaceAll(m, ",", ""))
			if err != nil {
				continue
			}
			out[name] = n
		}
	}
	return out
}
