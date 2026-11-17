package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// schemaVersion is the version this binary writes. It must equal the last entry
// of migrations (asserted by TestSchemaVersionMatchesLastStep).
const schemaVersion = 2

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
	for _, m := range migrations {
		if version >= m.version {
			continue
		}
		if err := runStep(ctx, db, m, sch); err != nil {
			return fmt.Errorf("migrate to schema version %d (%s): %w", m.version, m.name, err)
		}
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
	// Foreign keys were off while the parent tables were dropped and recreated, so
	// prove the children still point at real rows. Rows that were already orphaned
	// before the migration are reported, not fatal: refusing to start would leave
	// the operator no way in to fix them.
	if err := warnOrphanedRows(ctx, tx); err != nil {
		return err
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
	for _, c := range have {
		if !contains(want, c) {
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
