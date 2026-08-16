package hostsvc

import (
	"os"
	"testing"

	"github.com/taills/moduless/internal/dbtest"
)

// These tests TRUNCATE the per-plugin tables Core provisions, which the
// end-to-end coverage in tests/ also reads and writes. `go test ./...` runs
// the packages concurrently, so the database has to be taken exclusively.
// See internal/dbtest.
//
// No defer: os.Exit does not unwind. tests/testmain_defer_test.go enforces it.
func TestMain(m *testing.M) {
	unlock := dbtest.Lock()
	code := m.Run()
	unlock()
	os.Exit(code)
}
