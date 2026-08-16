// Package dbtest coordinates access to the shared test database.
//
// It is test-only but deliberately not a _test.go file: four separate packages
// need the same lock — core/auth, core/db, core/hostsvc and tests — and a
// _test.go file cannot be shared across packages. Nothing outside a test
// imports it, and internal/ makes that structural rather than a convention.
//
// # Why a lock at all
//
// These suites cannot be given per-run namespaces. Fixtures address rows by the
// plugin key from the plugin's own manifest — owner_key 'syncer', 'crasher',
// 'leaseholder' — and clear them with statements that name nothing else:
//
//	DELETE FROM plugin_queue WHERE owner_key = 'syncer'
//	TRUNCATE plugin_queue
//	TRUNCATE system_users RESTART IDENTITY CASCADE
//
// Those keys are the plugins' real identifiers and those tables are the ones
// Core provisions for them, so a per-run suffix would mean generating a fresh
// manifest for every run and thereby testing something other than what ships.
// Exclusive use of the database is an inherent requirement here, not an
// implementation detail that could be engineered away.
//
// # What it costs to leave unenforced
//
// `go test ./...` runs packages concurrently, so core/db's unqualified
// TRUNCATE plugin_queue lands in the middle of the queue tests in tests/.
// Reproduced deliberately by hammering core/db while the queue tests ran:
//
//	--- FAIL: TestQueueRedeliversAfterConsumerCrash
//	    the message was never redelivered after its consumer died;
//	    work handed to a plugin that crashed has been lost
//	--- FAIL: TestDeadMessagesDoNotShowInTheDepth
//	    dead depth = 2, want 3
//
// The first of those accuses the framework of losing work when a plugin
// crashes. It is not true, and the queue is not at fault — but that is a P0
// reading of a test that failed because another package emptied its table. The
// window is small (core/db finishes in about a second, tests runs for over
// three minutes) which is exactly why it surfaced as a rare unreproducible
// failure rather than as something anyone could chase down.
package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

const lockKey = 0x6d6f64756c65 // "module"

// Lock blocks until this process owns the test database and returns the
// release. It is a no-op when TEST_DATABASE_URL is unset, which is the same
// condition under which every database-backed test skips itself.
//
// Call it from TestMain without a defer:
//
//	func TestMain(m *testing.M) {
//		unlock := dbtest.Lock()
//		code := m.Run()
//		unlock()
//		os.Exit(code)
//	}
//
// os.Exit does not unwind, so a deferred unlock would not run. Postgres would
// release the lock anyway when the connection dropped, but the shape above is
// the one that stays correct when someone adds a second cleanup later — see
// tests/testmain_defer_test.go, which fails any TestMain that mixes the two.
func Lock() func() {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		return func() {}
	}

	pool, err := sql.Open("postgres", url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "locking the test database: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	// A pinned connection, not the pool: an advisory lock lives on the session
	// that took it, and the per-test handles are closed after each test.
	conn, err := pool.Conn(ctx)
	if err != nil {
		// Every database test skips when it cannot connect, so failing hard
		// here would turn "no database" into a build failure. Let the tests
		// report it themselves.
		fmt.Fprintf(os.Stderr, "test database unreachable, continuing unlocked: %v\n", err)
		pool.Close()
		return func() {}
	}

	var held bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&held); err != nil {
		fmt.Fprintf(os.Stderr, "locking the test database: %v\n", err)
		os.Exit(1)
	}
	if !held {
		// Say why the wait is happening, at the moment it happens. Without this
		// the package simply appears to hang before its first test — measured,
		// 434s against a normal 218s, with nothing on screen for the first half.
		notify("waiting for %s: another test package holds it.\n"+
			"These suites need the database to themselves — see internal/dbtest.\n",
			maskedURL(url))
		start := time.Now()
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
			fmt.Fprintf(os.Stderr, "waiting for the test database: %v\n", err)
			os.Exit(1)
		}
		notify("acquired after %s\n", time.Since(start).Round(time.Second))
	}

	return func() {
		_, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, lockKey)
		conn.Close()
		pool.Close()
	}
}

// notify writes somewhere the operator will actually see it.
//
// `go test` captures the test binary's stdout and stderr and prints them only
// when the package fails or when -v is given. Measured: with the lock held
// elsewhere, a plain run sat silent for 22s and showed nothing, while the same
// run with -v printed the message. A message that appears only under -v is no
// use here, because the person who needs it is the one watching an apparently
// hung terminal and deciding whether to interrupt it.
//
// So stderr keeps it in the record and /dev/tty puts it on the screen now. The
// tty open is best-effort: under CI, or with output redirected, there is no
// controlling terminal and stderr is the whole story. Verifying this needs a
// real pty — checking tty behaviour through a pipe returns a misleading
// negative — so `script -q /dev/null go test …` is how it was confirmed.
func notify(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
	if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		fmt.Fprintf(tty, format, args...)
		tty.Close()
	}
}

// maskedURL keeps the password out of the message. The URL is printed only to
// say which database is contended, and that needs the host and name, not the
// credentials.
func maskedURL(raw string) string {
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return raw
	}
	scheme := 0
	if i := strings.Index(raw, "://"); i >= 0 {
		scheme = i + 3
	}
	return raw[:scheme] + "***@" + raw[at+1:]
}
