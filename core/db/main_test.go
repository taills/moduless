package db

import (
	"os"
	"testing"

	"github.com/taills/moduless/internal/dbtest"
)

// These tests share one PostgreSQL instance with core/auth, core/hostsvc and
// tests/, and `go test ./...` runs those packages concurrently. This package
// is the most destructive of the four — it TRUNCATEs plugin_queue outright —
// so without the lock it empties the queue under the tests in tests/ that are
// counting messages, which then fail claiming Core loses work when a plugin
// crashes. See internal/dbtest for the reproduction.
//
// No defer: os.Exit does not unwind. tests/testmain_defer_test.go enforces it.
func TestMain(m *testing.M) {
	unlock := dbtest.Lock()
	code := m.Run()
	unlock()
	os.Exit(code)
}
