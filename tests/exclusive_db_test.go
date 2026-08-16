package tests

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

// This suite needs the test database to itself, and until now nothing said so.
//
// Fixtures address rows by the plugin key from their own manifest — owner_key
// 'syncer', 'crasher', 'leaseholder' — and clear them with statements that
// name nothing else: DELETE FROM plugin_queue WHERE owner_key = 'syncer',
// TRUNCATE ext_audit_audit_log, DELETE FROM ext_apikey_keys. Those keys are the
// plugins' real identifiers and those tables are the ones Core provisions for
// them, so neither can take a per-run suffix without generating a fresh
// manifest for every run and thereby testing something other than what ships.
//
// Two suites against one database therefore delete each other's rows mid-test
// and count each other's messages. Measured, running two at once: 19 failures
// across the pair, every one of them shaped like a queue bug —
//
//	listed 4 dead messages, want 2
//	dead depth = 6, want 3
//	the first publish reported itself as a duplicate
//
// The counts are exact doubles because there were exactly two writers. Nothing
// was wrong with the queue. That is what makes this worth enforcing rather than
// documenting: the failure is indistinguishable from a real defect, so the cost
// of leaving it is measured in hours spent debugging code that is correct.
//
// Sequential re-runs were never affected — each fixture clears its own rows on
// the way in — so `-count=2` passes and gives no warning.
const dbLockKey = 0x6d6f64756c65 // "module"

// lockTestDatabase blocks until this process owns the test database, and
// returns the release. It is a no-op when TEST_DATABASE_URL is unset, which is
// the same condition under which every database-backed test skips itself.
func lockTestDatabase() func() {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		return func() {}
	}

	// A dedicated pool, not one of the per-test handles: advisory locks live on
	// the session that took them, and requireDB closes its handle after each
	// test.
	pool, err := sql.Open("postgres", url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "locking the test database: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	conn, err := pool.Conn(ctx)
	if err != nil {
		// Unreachable for the suite proper: every database test skips when it
		// cannot connect, so failing hard here would turn "no database" into a
		// build failure. Let the tests report it themselves.
		fmt.Fprintf(os.Stderr, "test database unreachable, continuing unlocked: %v\n", err)
		pool.Close()
		return func() {}
	}

	var held bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, dbLockKey).Scan(&held); err != nil {
		fmt.Fprintf(os.Stderr, "locking the test database: %v\n", err)
		os.Exit(1)
	}
	if !held {
		// Say why the wait is happening, at the moment it happens. Without this
		// the suite simply appears to hang before its first test — measured,
		// 434s against a normal 218s, with nothing on screen for the first half.
		notify("another test suite holds %s; waiting for it to finish.\n"+
			"This suite needs the database to itself — see tests/exclusive_db_test.go.\n",
			maskedURL(url))
		start := time.Now()
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, dbLockKey); err != nil {
			fmt.Fprintf(os.Stderr, "waiting for the test database: %v\n", err)
			os.Exit(1)
		}
		notify("acquired after %s\n", time.Since(start).Round(time.Second))
	}

	return func() {
		_, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, dbLockKey)
		conn.Close()
		pool.Close()
	}
}

// notify writes somewhere the operator will actually see it.
//
// `go test` captures the test binary's stdout and stderr and prints them only
// when the package fails or when -v is given. Measured: with the lock held
// elsewhere, the plain run sat silent for 22s and showed nothing; the same run
// with -v printed the message. A message that appears only under -v is no use
// here, because the person who needs it is the one watching an apparently
// hung terminal and deciding whether to interrupt it.
//
// So stderr keeps it in the record, and /dev/tty puts it on the screen now.
// The tty open is best-effort: under CI, or with output redirected, there is no
// controlling terminal and stderr is the whole story.
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
