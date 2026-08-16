package auth

import (
	"os"
	"testing"

	"github.com/taills/moduless/internal/dbtest"
)

// These tests TRUNCATE system_users, which is shared with the login and admin
// coverage in tests/. `go test ./...` runs the packages concurrently, so the
// database has to be taken exclusively. See internal/dbtest.
//
// No defer: os.Exit does not unwind. tests/testmain_defer_test.go enforces it.
func TestMain(m *testing.M) {
	unlock := dbtest.Lock()
	code := m.Run()
	unlock()
	os.Exit(code)
}
