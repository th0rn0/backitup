package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
)

// schemaVersion is the version this binary writes. It must equal the last entry
// of migrations (asserted by TestSchemaVersionMatchesLastStep).
const schemaVersion = 7

// droppedColumns names, per table, the columns a migration is allowed to remove.
//
// canonicaliseTable rebuilds a table to schema.sql's definition and refuses to
// drop any column the file does not declare — an unexpected column means this
// database was written by something we do not know, and dropping it silently
// would be data loss. A column the project deliberately retires is drained
// through that same guard by name, so the rebuild that removes it is still the
// only mechanism and the "unknown column" refusal stays armed for everything
// else. An entry stays here forever: an old database may still carry the column,
// and every step before the one that drops it compares against today's
// schema.sql (there are no per-version snapshots of it).
var droppedColumns = map[string][]string{
	// v3: offsite retention belongs to the B2 bucket's lifecycle rule, so
	// backitup no longer keeps a horizon or a "don't prune" flag of its own.
	"clients": {"offsite_retention_days", "disable_offsite_pruning"},
}

// querier is the surface shared by *sql.DB and *sql.Tx, so the migration helpers
// can run either inside a step's transaction or on the bare handle.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// schemaTable is one CREATE TABLE statement from schema.sql.
type schemaTable struct {
	name string
	ddl  string
}

// schemaDef is schema.sql split into the two phases Open() applies it in: the
// tables first, then the indexes (which depend on the tables being canonical).
type schemaDef struct {
	tables  []schemaTable
	indexes []string
}

type migration struct {
	version int
	name    string
	// fkOff marks a step that rebuilds tables other tables reference. Dropping a
	// parent with foreign_keys on makes SQLite delete its children, which is
	// precisely the data loss this package exists to avoid.
	fkOff bool
	apply func(ctx context.Context, tx *sql.Tx, sch schemaDef) error
}

// migrations are the forward-only steps that bring an older database to
// schemaVersion. Each one is applied exactly once, inside a single transaction
// that also records PRAGMA user_version — so an interrupted step leaves the
// database exactly as it was, and a database already at (or beyond) a step is
// never inspected again, which is what makes a second Open() a no-op.
//
// Changing schema.sql therefore needs a new step (and a bumped schemaVersion):
// an existing database is only canonicalised by a step it has not applied yet.
// The new step's apply function can be applyCanonicalShape — the rebuild and the
// index pass are both idempotent and driven by comparing the file to the database.
//
// There are no down-migrations: a database written by a newer backitup is
// refused rather than silently mangled.
var migrations = []migration{
	{version: 1, name: "create tables", apply: applyCreateTables},
	{version: 2, name: "canonical table shape", fkOff: true, apply: applyCanonicalShape},
	{version: 3, name: "drop offsite retention columns", fkOff: true, apply: applyCanonicalShape},
	{version: 4, name: "mark unsupported remote backends", fkOff: true, apply: applyRemoteBackendsSupported},
	// v5: the rsync fidelity settings (sparse/hardlinks/ACLs/xattrs). Every
	// column defaults to 0, so an upgraded database keeps the snapshots it was
	// already producing; the settings are opt-in per client.
	{version: 5, name: "rsync fidelity settings", fkOff: true, apply: applyCanonicalShape},
	// v6: group_readable, the per-client switch that makes the rsync receiver
	// widen modes as it writes (--chmod=Dg+rx,Fg+r) so an account in the
	// server's group can read the tree. Defaults to 0: an upgraded database
	// keeps producing snapshots whose modes mirror the source.
	{version: 6, name: "group readable snapshots", fkOff: true, apply: applyCanonicalShape},
	// v7: client_storage, the per-client measurement of the hot store that the
	// lifecycle pass writes and the pages read. A table added after v1 needs its
	// CREATE in this step: applyCanonicalShape rebuilds a table that exists and
	// refuses one that does not (nothing in common to copy), so "canonicalise
	// everything" alone cannot introduce a table.
	{version: 7, name: "client disk usage", fkOff: true, apply: applyClientStorage},
}

// migrationFault is a test seam. When set, it is called at each point inside a
// step where a process could die, and a non-nil return aborts the step as if it
// had. Production code leaves it nil, so the calls cost one nil check.
var migrationFault func(point string) error

func fault(point string) error {
	if migrationFault == nil {
		return nil
	}
	return migrationFault(point)
}

// migrate brings the database to schemaVersion, one transaction per step.
func migrate(ctx context.Context, db *sql.DB) error {
	sch, err := parseSchema(schemaSQL)
	if err != nil {
		return err
	}
	version, err := userVersion(ctx, db)
	if err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than this backitup supports (%d): "+
			"upgrade backitup instead of running an older one against it", version, schemaVersion)
	}
	var ran bool
	for _, m := range migrations {
		if version >= m.version {
			continue
		}
		if err := runStep(ctx, db, m, sch); err != nil {
			return fmt.Errorf("migrate to schema version %d (%s): %w", m.version, m.name, err)
		}
		ran = true
	}
	// Reported once for the migration as a whole, not once per rebuilt table: FKs
	// were off while parent tables were dropped and recreated, so prove the
	// children still point at real rows. A database that needed no step is not
	// re-checked — a steady-state start must stay silent.
	if ran {
		return warnOrphanedRows(ctx, db)
	}
	return nil
}

// runStep applies one migration inside a single transaction and records the new
// version in that same transaction. PRAGMA user_version is part of the database
// header and is rolled back with everything else, so the version can never claim
// work that did not commit.
func runStep(ctx context.Context, db *sql.DB, m migration, sch schemaDef) error {
	if m.fkOff {
		// PRAGMA foreign_keys is a no-op inside a transaction, so it has to be
		// toggled around it. See the doc comment on migration.fkOff.
		if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
			return fmt.Errorf("disable foreign_keys: %w", err)
		}
	}
	err := func() error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
		if err := m.apply(ctx, tx, sch); err != nil {
			return err
		}
		// The step's work and its version stamp commit together or not at all.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			return fmt.Errorf("record user_version %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		return nil
	}()
	if m.fkOff {
		if _, fkErr := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); fkErr != nil {
			if err == nil {
				return fmt.Errorf("re-enable foreign_keys: %w", fkErr)
			}
			// Report the migration failure, not the cleanup one, but say both.
			return fmt.Errorf("%w (and foreign_keys could not be re-enabled: %v)", err, fkErr)
		}
	}
	return err
}

func userVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	return v, nil
}

// applyCreateTables creates every table schema.sql defines that does not exist
// yet. Existing tables are left alone here: they are rebuilt, not patched, by
// applyCanonicalShape.
func applyCreateTables(ctx context.Context, tx *sql.Tx, sch schemaDef) error {
	for _, t := range sch.tables {
		if _, err := tx.ExecContext(ctx, t.ddl); err != nil {
			return fmt.Errorf("create table %s: %w", t.name, err)
		}
	}
	return nil
}

// applyCanonicalShape makes every table physically identical to schema.sql, then
// creates the indexes. It runs before any index exists or is relied on: a rebuild
// drops the old table and its indexes with it, and idx_clients_slug needs the slug
// backfill to have run first.
func applyCanonicalShape(ctx context.Context, tx *sql.Tx, sch schemaDef) error {
	// Create first, canonicalise second. schema.sql describes the shape THIS
	// build expects, so it also lists tables a later step introduces — and
	// canonicaliseTable refuses a table that does not exist yet. Without this
	// call, a step at version N fails on a table added at version N+1: a
	// database at v5 could not reach v7 because step 6 met client_storage, which
	// only v7 creates. Every DDL in schema.sql is CREATE TABLE IF NOT EXISTS, so
	// this is a no-op for tables that already exist.
	if err := applyCreateTables(ctx, tx, sch); err != nil {
		return err
	}
	for _, t := range sch.tables {
		if err := canonicaliseTable(ctx, tx, t); err != nil {
			return err
		}
	}
	if err := backfillClientSlugs(ctx, tx); err != nil {
		return fmt.Errorf("backfill client slugs: %w", err)
	}
	for _, idx := range sch.indexes {
		if _, err := tx.ExecContext(ctx, idx); err != nil {
			return fmt.Errorf("create index: %w", err)
		}
	}
	return nil
}

// applyClientStorage adds the client_storage table (v7) and brings every other
// table to its canonical shape. The create comes first because a table that does
// not exist cannot be canonicalised — see the step's comment.
func applyClientStorage(ctx context.Context, tx *sql.Tx, sch schemaDef) error {
	if err := applyCreateTables(ctx, tx, sch); err != nil {
		return err
	}
	return applyCanonicalShape(ctx, tx, sch)
}

// applyRemoteBackendsSupported brings the remotes table to its canonical shape
// (which adds `unsupported` with its default of 0) and then marks every row this
// build cannot use.
//
// The rows are kept, not deleted: an operator's configured remote is their
// configuration, and a migration that quietly drops it would be indistinguishable
// from a bug that lost it. What changes is that nothing uses an unsupported
// remote any more — the lifecycle skips it and the UI says to recreate it as B2 —
// so leaving the row behind cannot keep a non-B2 destination in service.
func applyRemoteBackendsSupported(ctx context.Context, tx *sql.Tx, sch schemaDef) error {
	if err := applyCanonicalShape(ctx, tx, sch); err != nil {
		return err
	}
	// b2 is the only provider; crypt is the client-side encryption wrapper
	// around one, and stays supported (see model.Backends).
	if _, err := tx.ExecContext(ctx,
		`UPDATE remotes SET unsupported = 1 WHERE backend NOT IN ('b2', 'crypt')`); err != nil {
		return fmt.Errorf("mark unsupported remotes: %w", err)
	}
	return nil
}

// warnOrphanedRows reports rows whose parent row is missing (PRAGMA
// foreign_key_check), naming a few of them for the operator.
func warnOrphanedRows(ctx context.Context, q querier) error {
	var total int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&total); err != nil {
		return fmt.Errorf("check foreign keys: %w", err)
	}
	if total == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, `SELECT "table", rowid, "parent" FROM pragma_foreign_key_check LIMIT 5`)
	if err != nil {
		return fmt.Errorf("check foreign keys: %w", err)
	}
	defer rows.Close()
	var examples []string
	for rows.Next() {
		var table, parent string
		var rowid int64
		if err := rows.Scan(&table, &rowid, &parent); err != nil {
			return fmt.Errorf("check foreign keys: %w", err)
		}
		examples = append(examples, fmt.Sprintf("%s.rowid=%d has no %s row", table, rowid, parent))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("check foreign keys: %w", err)
	}
	warnf("WARNING: %d row(s) reference a parent row that does not exist (%s). The migration kept them "+
		"as they were; delete or re-link them, because the dashboard and the pruners read every row.",
		total, strings.Join(examples, "; "))
	return nil
}

// ── debris from an interrupted migration ────────────────────────────────────
//
// Before migrations were transactional (see the history this file's steps
// replace), the runs table was rebuilt by running CREATE runs_new / INSERT /
// DROP TABLE runs / ALTER TABLE runs_new RENAME TO runs one statement at a time.
// A process that died between the DROP and the RENAME left every run in
// `runs_new` and none in `runs`. That build also kept no user_version, so nothing
// records the interruption: the database opens cleanly, the dashboard shows an
// empty history, and the rows sit in a table no query mentions. The same shape
// can be left today by an interrupted swap under the name `<table>__canonical`,
// although the steps being transactional makes that unreachable from this build.
//
// These tables are reported, never repaired. Which of two tables is current is a
// judgement only the operator can make; copying rows across automatically would
// fabricate history if it guessed wrong. The receipt for that decision is one
// warning line at startup.

// strayTable is a table an interrupted migration left behind: its name, and the
// schema.sql table whose history it may be holding ("" when no table of that name
// is defined, in which case only the stray itself is named).
type strayTable struct {
	name string
	live string
}

// warnStrandedMigrationTables reports, one line per table, every table that
// schema.sql does not define and that only a crashed migration could have
// created, along with how many rows it still holds.
//
// It is called after migrate() has returned — outside every step transaction —
// and is strictly read-only: one read of sqlite_master, then one COUNT(*) per
// stray table. It cannot fail its caller: an error is logged and startup
// continues, because refusing to start over a diagnostic would turn a stale table
// into an outage. A database with no such table produces no output at all, so a
// line in the log always means rows really are stranded.
func warnStrandedMigrationTables(ctx context.Context, db *sql.DB) {
	strays, err := strandedTables(ctx, db)
	if err != nil {
		log.Printf("store: could not check for tables left behind by an interrupted migration: %v", err)
		return
	}
	for _, stray := range strays {
		var rows int64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoteIdent(stray.name)).Scan(&rows); err != nil {
			log.Printf("store: table %q looks like debris from an interrupted migration but its rows "+
				"could not be counted: %v", stray.name, err)
			continue
		}
		warnStrandedTable(stray, rows)
	}
}

// strandedTables returns the tables in the database that schema.sql does not
// define and that only an interrupted migration could have created, oldest name
// first so the log order is stable.
func strandedTables(ctx context.Context, db *sql.DB) ([]strayTable, error) {
	sch, err := parseSchema(schemaSQL)
	if err != nil {
		return nil, err
	}
	defined := make(map[string]struct{}, len(sch.tables))
	for _, t := range sch.tables {
		defined[t.name] = struct{}{}
	}
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("read sqlite_master: %w", err)
	}
	defer rows.Close()
	var out []strayTable
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read sqlite_master: %w", err)
		}
		if _, ok := defined[name]; ok {
			continue // a table this backitup uses
		}
		live, ok := migrationDebrisOf(name)
		if !ok {
			continue // the operator's own table: not ours to comment on
		}
		if _, ok := defined[live]; !ok {
			live = "" // nothing this schema knows: name only the stray
		}
		out = append(out, strayTable{name: name, live: live})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read sqlite_master: %w", err)
	}
	return out, nil
}

// migrationDebrisOf reports whether a table name is one an interrupted migration
// can leave behind, and which table it was standing in for. Exactly two shapes
// have ever existed: `runs_new`, the temporary name the pre-transactional swap
// used, and `<table>__canonical`, the one the swap uses now. Nothing else
// qualifies — warning about every table this schema does not know would fire on
// tables an operator created on purpose, and a warning that is always there is a
// warning nobody reads.
func migrationDebrisOf(name string) (live string, ok bool) {
	if name == "runs_new" {
		return "runs", true
	}
	if base, found := strings.CutSuffix(name, "__canonical"); found && base != "" {
		return base, true
	}
	return "", false
}

// warnStrandedTable reports one stranded table. It is one line so it is greppable
// in startup output ("WARNING: table "runs_new" holds"), and it says plainly that
// nothing was copied: the operator must not read a silent rescue into the fact
// that the server came up.
func warnStrandedTable(stray strayTable, rows int64) {
	if stray.live == "" {
		warnf("WARNING: table %q holds %d row(s) that backitup does not read: it was left behind by an "+
			"older backitup that crashed in the middle of a schema migration (migrations were not "+
			"transactional then), and no query in this build mentions it. Nothing was renamed, dropped "+
			"or copied. Inspect it by hand and restore what it holds yourself — backitup will not, "+
			"because it cannot tell which of two tables is current and a wrong guess would invent history.",
			stray.name, rows)
		return
	}
	warnf("WARNING: table %q holds %d row(s) that backitup does not read: it was left behind by an "+
		"older backitup that crashed in the middle of a schema migration (migrations were not "+
		"transactional then), and every query — the dashboard, the pruners, the API — reads %q "+
		"instead, so this history is invisible while the server otherwise runs normally. Nothing was "+
		"renamed, dropped or copied into %q: backitup cannot tell which of the two tables is current "+
		"and a wrong guess would invent history. Inspect both by hand and, if %q is the current one, "+
		"restore it yourself (e.g. copy its rows into %q); backitup will not.",
		stray.name, rows, stray.live, stray.live, stray.name, stray.live)
}

// quoteIdent quotes a table or column name for interpolation into SQL. The names
// come from sqlite_master, but a quoted identifier costs nothing and keeps a name
// containing a quote from turning a diagnostic into a syntax error.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// canonicaliseTable rebuilds one table to the definition schema.sql gives it, or
// does nothing when its stored definition is already identical. The comparison is
// on sqlite_master's own copy of the CREATE statement — SQLite keeps the text it
// was given, minus "IF NOT EXISTS" — so "equal" means the same columns in the same
// order with the same constraints.
//
// The rebuild is the SQLite table-swap: create the canonical table under a
// temporary name, copy the columns both sides share (by name, never by position),
// drop the old table, rename the new one into place. It runs inside the caller's
// transaction, which is what makes an interrupt at any point harmless.
func canonicaliseTable(ctx context.Context, tx querier, t schemaTable) error {
	tmp := t.name + "__canonical"
	canonical := strings.Replace(t.ddl, "CREATE TABLE IF NOT EXISTS "+t.name, "CREATE TABLE "+t.name, 1)
	if canonical == t.ddl {
		return fmt.Errorf("schema.sql: table %s: want a plain CREATE TABLE IF NOT EXISTS, got %q", t.name, firstLine(t.ddl))
	}
	stored, err := storedDDL(ctx, tx, t.name)
	if err != nil {
		return err
	}
	if normaliseDDL(stored) == normaliseDDL(canonical) {
		return nil // already canonical: same columns, same order, same constraints
	}
	create := strings.Replace(canonical, "CREATE TABLE "+t.name, "CREATE TABLE "+tmp, 1)

	if _, err := tx.ExecContext(ctx, create); err != nil {
		return fmt.Errorf("create canonical %s: %w", t.name, err)
	}
	if err := fault("created:" + t.name); err != nil {
		return err
	}
	have, err := tableColumns(ctx, tx, t.name)
	if err != nil {
		return err
	}
	want, err := tableColumns(ctx, tx, tmp)
	if err != nil {
		return err
	}
	// A rebuild copies the columns both sides share, so a column that exists only
	// in the database is dropped. That is data loss unless the project retired
	// the column on purpose, so refuse — except for the columns named in
	// droppedColumns, which is how a retirement is expressed.
	retired := droppedColumns[t.name]
	for _, c := range have {
		if !contains(want, c) && !contains(retired, c) {
			// Dropping a column nobody asked us to drop is data loss. Refuse
			// instead: this database was written by something we do not know.
			return fmt.Errorf("table %s: existing column %q is not in the canonical schema; "+
				"refusing to rebuild it and drop that data", t.name, c)
		}
	}

	cols := sharedColumns(want, have)
	if len(cols) == 0 {
		return fmt.Errorf("table %s: no column in common with the canonical schema", t.name)
	}
	// The column names come from our own schema, but quote them anyway: a column
	// that happens to be a keyword must not turn a migration into a syntax error.
	identList := `"` + strings.Join(cols, `", "`) + `"`
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s", tmp, identList, identList, t.name)); err != nil {
		return fmt.Errorf("copy %s rows: %w", t.name, err)
	}
	if err := fault("copied:" + t.name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE "+t.name); err != nil {
		return fmt.Errorf("drop old %s: %w", t.name, err)
	}
	// The dangerous point in the old, ungrouped migration: the rows are now only
	// in <table>__canonical and the original table is gone.
	if err := fault("dropped:" + t.name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE "+tmp+" RENAME TO "+t.name); err != nil {
		return fmt.Errorf("rename canonical %s: %w", t.name, err)
	}
	// A rebuild voids the indexes of that table; applyCanonicalShape recreates
	// them from schema.sql once every table is canonical.
	return fault("rebuilt:" + t.name)
}

func storedDDL(ctx context.Context, tx querier, table string) (string, error) {
	var ddl string
	err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&ddl)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("table %s is missing from the schema", table)
	}
	if err != nil {
		return "", fmt.Errorf("read schema for %s: %w", table, err)
	}
	return ddl, nil
}

// tableColumns returns a table's column names in physical order (PRAGMA
// table_info), which is exactly what a positional copy depends on.
func tableColumns(ctx context.Context, tx querier, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", table, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read columns of %s: %w", table, err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", table, err)
	}
	return cols, nil
}

// sharedColumns returns the columns of want that also exist in have, in want's
// order: the columns to copy, and the order the INSERT writes them in.
func sharedColumns(want, have []string) []string {
	var out []string
	for _, w := range want {
		if contains(have, w) {
			out = append(out, w)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// normaliseDDL makes two CREATE statements comparable without letting formatting
// decide: comments are dropped and runs of whitespace collapse to one space, both
// only outside quoted literals (a DEFAULT 'a  b' must keep its spaces).
func normaliseDDL(s string) string {
	var b strings.Builder
	space := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			j := i + 1
			for j < len(s) {
				if s[j] == '\'' {
					if j+1 < len(s) && s[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			if j > len(s) {
				j = len(s)
			}
			b.WriteString(s[i:j])
			space = false
			i = j - 1
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			space = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			space = true
		default:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}

// parseSchema splits schema.sql into tables and indexes. Statements are executed
// in this order, and the text of each table statement is what a database is
// compared against, so the file is the only place a table is defined.
func parseSchema(script string) (schemaDef, error) {
	var sch schemaDef
	for _, stmt := range splitSQL(script) {
		switch {
		case strings.HasPrefix(stmt, "CREATE TABLE "):
			rest := strings.TrimPrefix(stmt, "CREATE TABLE ")
			rest = strings.TrimPrefix(rest, "IF NOT EXISTS ")
			name, _, _ := strings.Cut(rest, " ")
			name = strings.TrimSpace(strings.TrimSuffix(name, "("))
			if name == "" {
				return sch, fmt.Errorf("schema.sql: cannot read a table name from %q", firstLine(stmt))
			}
			sch.tables = append(sch.tables, schemaTable{name: name, ddl: stmt})
		case strings.HasPrefix(stmt, "CREATE INDEX "), strings.HasPrefix(stmt, "CREATE UNIQUE INDEX "):
			sch.indexes = append(sch.indexes, stmt)
		default:
			return sch, fmt.Errorf("schema.sql: unrecognised statement %q", firstLine(stmt))
		}
	}
	if len(sch.tables) == 0 {
		return sch, fmt.Errorf("schema.sql: no CREATE TABLE statements")
	}
	return sch, nil
}

// splitSQL returns the statements of a script, each trimmed and without the
// comments that precede its first token (SQLite does not keep those either).
// Comments and semicolons inside quoted literals are left alone, and a comment
// inside a statement is kept: it documents the column it sits next to in
// sqlite_master, and comparing two statements ignores comments anyway.
func splitSQL(script string) []string {
	var (
		out     []string
		cur     strings.Builder
		hasText bool
	)
	finish := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
		hasText = false
	}
	for i := 0; i < len(script); i++ {
		c := script[i]
		switch {
		case c == '\'':
			j := i + 1
			for j < len(script) {
				if script[j] == '\'' {
					if j+1 < len(script) && script[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			if j > len(script) {
				j = len(script)
			}
			cur.WriteString(script[i:j])
			hasText = true
			i = j - 1
		case c == '-' && i+1 < len(script) && script[i+1] == '-':
			j := i
			for j < len(script) && script[j] != '\n' {
				j++
			}
			if hasText {
				cur.WriteString(script[i:j])
			}
			i = j - 1
		case c == ';':
			finish()
		default:
			cur.WriteByte(c)
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				hasText = true
			}
		}
	}
	finish()
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
