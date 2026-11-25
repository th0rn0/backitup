package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// migrateToVersion applies the real steps up to and including target, so a test
// can stand up the database an older release shipped.
func migrateToVersion(t *testing.T, db *sql.DB, target int) {
	t.Helper()
	ctx := context.Background()
	sch, err := parseSchema(schemaSQL)
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	for _, m := range migrations {
		if m.version > target {
			break
		}
		if err := runStep(ctx, db, m, sch); err != nil {
			t.Fatalf("apply step %d (%s): %v", m.version, m.name, err)
		}
	}
	if got := scalarInt(t, db, `PRAGMA user_version`); got != target {
		t.Fatalf("fixture is at user_version %d, want %d", got, target)
	}
}

// A database left by a build that predates client_storage must still upgrade.
//
// The canonical-shape steps (v2, v3, v5, v6) walk the tables in the *current*
// schema.sql, and that file lists tables a later step introduces. Step 6 used to
// meet client_storage, which only v7 creates, and refuse the whole migration:
//
//	open store: migrate: migrate to schema version 6 (group readable snapshots):
//	table client_storage is missing from the schema
//
// The server then never starts, so a deployment at v5 or below cannot move to v7
// at all. applyCanonicalShape now creates missing tables before canonicalising
// them, and this test holds that ordering in place.
func TestUpgradeFromAnOlderVersionAddsClientStorage(t *testing.T) {
	for _, from := range []int{5, 6} {
		t.Run(map[int]string{5: "v5", 6: "v6"}[from], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "upgrade.db")
			db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer db.Close()

			migrateToVersion(t, db, from)
			// The current DDL creates every table, including ones a release of
			// this version had never heard of. Drop it so the fixture matches
			// what an older build actually left on disk: no client_storage.
			if _, err := db.Exec(`DROP TABLE IF EXISTS client_storage`); err != nil {
				t.Fatalf("drop client_storage to model the older shape: %v", err)
			}

			if err := migrate(context.Background(), db); err != nil {
				t.Fatalf("upgrade from v%d to v%d failed: %v", from, schemaVersion, err)
			}
			if got := scalarInt(t, db, `PRAGMA user_version`); got != schemaVersion {
				t.Fatalf("after upgrade user_version = %d, want %d", got, schemaVersion)
			}
			// The table the migration introduced has to be usable, not merely
			// present: select the columns the lifecycle writes.
			rows, err := db.Query(`SELECT client_id, disk_bytes, disk_partial, measured_at FROM client_storage`)
			if err != nil {
				t.Fatalf("client_storage is not usable after the upgrade: %v", err)
			}
			rows.Close()
		})
	}
}
