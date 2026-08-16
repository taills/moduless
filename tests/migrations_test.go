package tests

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/taills/moduless/core/db"
)

// Every migration applies, reverses, and applies again.
//
// RunMigrations only ever calls Up, and nothing anywhere calls Down — so the
// eleven .down.sql files in the repository have never been executed by Core or
// by a test. They look like a rollback capability. Whether they are one would
// be discovered by whoever first needed to roll back, at the moment they
// needed it.
//
// Up-down-up rather than just down, because a down that drops too much leaves
// a database the next Up cannot repair, and that is the failure that costs
// data rather than time.
//
// Against a throwaway database: running Down against the shared test database
// would take the schema out from under every other test in this package.
func TestMigrationsRoundTrip(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	admin, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer admin.Close()

	const scratch = "moduless_migration_roundtrip"
	// Dropped first in case an earlier run was interrupted: a leftover
	// database would make this test pass or fail on the previous run's state.
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + scratch); err != nil {
		t.Skipf("cannot manage databases as this user: %v", err)
	}
	if _, err := admin.Exec(`CREATE DATABASE ` + scratch); err != nil {
		t.Skipf("cannot create a scratch database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + scratch)
	})

	scratchURL := replaceDatabase(url, scratch)
	conn, err := sql.Open("postgres", scratchURL)
	if err != nil {
		t.Fatalf("connecting to the scratch database: %v", err)
	}
	defer conn.Close()

	if err := db.RunMigrations(conn); err != nil {
		t.Fatalf("first Up: %v", err)
	}

	// A table each migration set is responsible for, so "the schema is there"
	// is checked rather than assumed from the absence of an error.
	for _, table := range []string{"system_users", "audit_logs", "plugin_queue", "plugin_config"} {
		if !tableExists(t, conn, table) {
			t.Fatalf("%s is missing after Up; the migrations did not build the schema "+
				"they are supposed to", table)
		}
	}

	// The reversible window: everything added after the one-way door at 000008.
	//
	// Counted from the directory rather than written down. A constant here
	// would still pass after somebody adds 000012 — it would simply stop
	// covering the newest migration, which is the one most likely to have a
	// rollback nobody has tried.
	reversible := migrationsAfter(t, 8)
	if reversible == 0 {
		t.Fatal("no migrations after the one-way door, so this covers nothing")
	}
	if err := db.RollBackMigrations(conn, reversible); err != nil {
		t.Fatalf("rolling back %d migration(s): %v; the .down.sql files are not a "+
			"rollback anybody can use", reversible, err)
	}
	if tableExists(t, conn, "plugin_config") {
		t.Error("plugin_config survived its own rollback; a down migration that leaves " +
			"its table behind makes the next Up fail on an object that already exists")
	}

	// The half that matters more than the rollback itself: after going back,
	// the schema can be built again. A down that drops too much leaves a
	// database no Up can repair.
	if err := db.RunMigrations(conn); err != nil {
		t.Fatalf("re-applying after a rollback: %v", err)
	}
	for _, table := range []string{"plugin_config", "audit_logs"} {
		if !tableExists(t, conn, table) {
			t.Errorf("%s is missing after re-applying; the rollback left the database "+
				"in a state the migrations cannot rebuild", table)
		}
	}
	// And the column 000011 adds, so the round trip is checked at the level the
	// migration actually works at rather than at the table it belongs to.
	if !columnExists(t, conn, "audit_logs", "trace_id") {
		t.Error("audit_logs.trace_id did not come back; the round trip restored the " +
			"table without what the migration added to it")
	}
}

// Rolling back past 000008 stops there, with a reason.
//
// It dropped the extension tables and cannot put the data back, so it declines
// — but it used to decline by doing nothing, and `migrate down` does not stop
// at a migration that does nothing. It carried on, and 000005 then tried to
// ALTER a table 000008 had dropped. The operator saw `relation "extensions"
// does not exist`, three migrations away from the decision that caused it.
func TestRollingBackPastTheOneWayDoorSaysWhy(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	admin, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer admin.Close()

	const scratch = "moduless_migration_oneway"
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + scratch); err != nil {
		t.Skipf("cannot manage databases as this user: %v", err)
	}
	if _, err := admin.Exec(`CREATE DATABASE ` + scratch); err != nil {
		t.Skipf("cannot create a scratch database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + scratch) })

	conn, err := sql.Open("postgres", replaceDatabase(url, scratch))
	if err != nil {
		t.Fatalf("connecting to the scratch database: %v", err)
	}
	defer conn.Close()

	if err := db.RunMigrations(conn); err != nil {
		t.Fatalf("Up: %v", err)
	}

	err = db.RollBackMigrations(conn, 0) // all the way
	if err == nil {
		t.Fatal("a full rollback reported success; it cannot have restored the " +
			"extension tables' data, so reporting success is the one answer that " +
			"is certainly wrong")
	}
	if !strings.Contains(err.Error(), "000008") {
		t.Errorf("the failure does not name the migration that refused: %v", err)
	}
	if !strings.Contains(err.Error(), "backup") {
		t.Errorf("the failure does not say what to do instead: %v", err)
	}
}

func columnExists(t *testing.T, conn *sql.DB, table, column string) bool {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`,
		table, column).Scan(&n)
	if err != nil {
		t.Fatalf("checking for %s.%s: %v", table, column, err)
	}
	return n > 0
}
func tableExists(t *testing.T, conn *sql.DB, name string) bool {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_name = $1`, name).Scan(&n)
	if err != nil {
		t.Fatalf("checking for %s: %v", name, err)
	}
	return n > 0
}

// replaceDatabase swaps the database name in a postgres URL, keeping
// everything else — credentials, host, options — as it was.
func replaceDatabase(url, name string) string {
	slash := strings.LastIndex(url, "/")
	if slash < 0 {
		return url
	}
	rest := url[slash+1:]
	if q := strings.Index(rest, "?"); q >= 0 {
		return url[:slash+1] + name + rest[q:]
	}
	return url[:slash+1] + name
}

// migrationsAfter counts the up migrations numbered above n.
func migrationsAfter(t *testing.T, n int) int {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join("..", "core", "db", "migrations"))
	if err != nil {
		t.Fatalf("reading the migrations directory: %v", err)
	}
	count := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		num, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			t.Fatalf("migration %s does not start with a number", name)
		}
		if num > n {
			count++
		}
	}
	return count
}
