package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	sqlite "modernc.org/sqlite"
)

// Carrying the ledger across a library root rename (`holdfast reroot`).
//
// Every path this ledger holds is ABSOLUTE and names a library root as written in the
// configuration that recorded it. When an operator mounts the same filesystem at a new
// path and edits library_roots to match, nothing on disk changed but every row still names
// the old tree: a scan never enumerates those paths again, so a deterministic failure is
// encoded and failed once more under the new name, a done or skipped verdict is taken
// again, and a retention recorded under the old path is out of reach of the release and of
// `holdfast restore`.
//
// This file is the ledger half of the fix: which columns hold a path, what rewriting the
// rows under one tree to another would do, and the rewrite itself, in ONE transaction. It
// never touches the filesystem - this package records state and never reads a file - so
// the evidence that the new tree holds the same data is taken by the caller, from the
// fingerprints PlanReroot hands it.

// ErrLedgerInUse is returned by OpenExclusive when another connection holds the ledger:
// a `serve` or `run` daemon, a worker's server, or another local command.
var ErrLedgerInUse = errors.New("the ledger is open in another process")

// OpenExclusive opens an EXISTING ledger as its ONLY user, for a local command that must
// not run beside a daemon, and holds it that way until Close.
//
// The lock is SQLite's own rather than a file this package invents, so it needs nothing
// from the daemon and covers every build that ever opened the file. A WAL-mode connection
// holds a SHARED lock on the database file for as long as it is open, idle or not, and
// locking_mode=EXCLUSIVE makes this handle take an EXCLUSIVE lock on its first transaction
// and keep it until it closes. So the open is refused while anything else has the ledger
// open, and while this handle is open a daemon that starts cannot read the ledger at all:
// its open waits out its busy timeout and fails, rather than scanning a ledger whose paths
// are being rewritten underneath it.
//
// Everything else is Open's: it migrates a ledger an earlier build wrote, because the
// command it serves writes through this build's schema, and it reports what it applied
// through MigrationReport. It never creates a ledger - a command that answered "nothing to
// rewrite" by writing an empty database into the state directory would be creating the
// thing it was asked about.
func OpenExclusive(path string) (*SQLite, error) { return openExclusive(path, true) }

// OpenExclusiveCurrent is OpenExclusive for a caller that must change NOTHING: it holds the
// ledger the same way, and refuses a ledger whose schema is not this build's, in either
// direction, rather than migrating it (ErrSchemaBehind for an older one). A dry run is its
// caller. A migration stamps a version the holdfast that wrote the ledger then refuses to
// open, so a command that promised to change nothing would have stranded that daemon.
//
// It refuses rather than migrating inside a transaction it rolls back because migrate
// commits each step in a transaction of its own: a rolled-back variant would be a second
// migration path that only the dry run ever ran.
func OpenExclusiveCurrent(path string) (*SQLite, error) { return openExclusive(path, false) }

// ErrSchemaBehind is a ledger an earlier build wrote, refused by a door that does not
// migrate.
var ErrSchemaBehind = errors.New("the ledger's schema is older than this build's")

func openExclusive(path string, upgrade bool) (*SQLite, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("store: open %q exclusively: %w", path, err)
	}
	// journal_mode is deliberately absent. The driver applies busy_timeout first and the
	// rest in lexical order, so journal_mode(WAL) would read the database BEFORE
	// locking_mode is set, and the handle would open the WAL in shared mode. The mode is
	// recorded in the file itself: a ledger Open wrote is WAL already.
	dsn := fmt.Sprintf("file:%s?mode=rw&_pragma=busy_timeout(1000)&_pragma=locking_mode(EXCLUSIVE)"+
		"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)", uriPath(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q exclusively: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// The lock is TAKEN here, by a write transaction, rather than whenever the first
	// statement happens to run: an open that returned before it held the ledger would be a
	// claim this function did not make good.
	if err := takeExclusive(db); err != nil {
		_ = db.Close()
		if busy(err) {
			return nil, fmt.Errorf("%w: %s (%v)", ErrLedgerInUse, path, err)
		}
		return nil, fmt.Errorf("store: open %q exclusively: %w", path, err)
	}
	if !upgrade {
		have, err := readSchemaVersion(context.Background(), db)
		if err == nil {
			switch want := schemaVersion(); {
			case have > want:
				err = errSchemaFromTheFuture(have, want)
			case have < want:
				err = fmt.Errorf("%w: it is at schema version %d and this build reads version %d", ErrSchemaBehind, have, want)
			}
		}
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		return &SQLite{db: db}, nil
	}
	applied, err := migrate(context.Background(), db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLite{db: db, applied: applied}, nil
}

func takeExclusive(db *sql.DB) error {
	if _, err := db.Exec(`BEGIN EXCLUSIVE`); err != nil {
		return err
	}
	_, err := db.Exec(`COMMIT`)
	return err
}

// busy reports whether err is SQLite saying another connection holds a lock it needs.
func busy(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() & 0xff { // the primary result code under any extended one
	case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED
		return true
	}
	return false
}

// RerootColumn names one column that holds a library path.
type RerootColumn struct {
	Table, Column string
}

// String renders the column as table.column.
func (c RerootColumn) String() string { return c.Table + "." + c.Column }

// rerootTable is one table that holds paths: the columns a reroot rewrites, and the key
// whose uniqueness decides what a collision is.
type rerootTable struct {
	name string
	// columns are the TEXT columns whose whole value is a path.
	columns []string
	// key is the path column in the table's uniqueness constraint, "" where no
	// constraint involves a path (swap_incidents, keyed by an id).
	key string
	// keyWith is the column the constraint pairs key with, "" where the path is the
	// whole key. Two rows at one path are then legal - a superseded jobs row beside the
	// current one, two sweeps' checks of one file - and only the same pair collides.
	keyWith string
}

// sidecarColumn is the one path that is not a whole column value: the published sidecar
// each element of jobs.subtitle_sidecars names. It is rewritten through the column's own
// codec (ParseSidecars, Encode), never by text substitution inside the JSON.
var sidecarColumn = RerootColumn{Table: "jobs", Column: "subtitle_sidecars"}

// rerootTables is EVERY column in this schema that holds a library path, in the order a
// reroot rewrites them. internal/store's TestRerootColumns_EveryColumnIsClassified reads
// the schema itself and fails on a column that is in neither this list nor its list of
// columns that hold no path, so a migration that adds a path column cannot land without
// deciding which it is.
//
// Free-text columns (a failure's reason, a swap's error) are not here although an error
// text can quote a path: they are what was said at the time, about the path as it was,
// and rewriting prose would be editing the record rather than carrying it.
var rerootTables = []rerootTable{
	{name: "jobs", columns: []string{"path", "target_path", "library_root"}, key: "path", keyWith: "fingerprint"},
	{name: "retained_originals", columns: []string{"source_path", "swapped_path", "retained_path"}, key: "source_path"},
	{name: "swap_incidents", columns: []string{"source_path", "replacement_path"}},
	{name: "path_exclusions", columns: []string{"path"}, key: "path"},
	{name: "health_checks", columns: []string{"path"}, key: "path", keyWith: "sweep_id"},
	{name: "node_leases", columns: []string{"path", "temp_path"}, key: "path", keyWith: "epoch"},
}

// RerootColumns lists every path-bearing column a reroot rewrites, sidecar paths included.
func RerootColumns() []RerootColumn {
	var out []RerootColumn
	for _, t := range rerootTables {
		for _, c := range t.columns {
			out = append(out, RerootColumn{Table: t.name, Column: c})
		}
		if t.name == sidecarColumn.Table {
			out = append(out, sidecarColumn)
		}
	}
	return out
}

// ErrRerootPaths is a from or to a reroot cannot be defined over.
var ErrRerootPaths = errors.New("store: reroot paths")

// RerootPath is where p lies once the tree at from is carried to to, and whether p lies in
// that tree at all. The comparison is on PATH COMPONENTS, never a string prefix:
// /media/old2 is not under /media/old.
func RerootPath(p, from, to string) (string, bool) {
	if p == from {
		return to, true
	}
	if strings.HasPrefix(p, from+"/") {
		return to + p[len(from):], true
	}
	return "", false
}

// checkRerootPaths refuses a pair the rewrite is not well defined over. The caller checks
// them against the configuration; these are the rules the arithmetic itself needs, held
// here too so no caller can hand the rewrite a pair that would merge two trees.
func checkRerootPaths(from, to string) error {
	for _, p := range []struct{ name, v string }{{"from", from}, {"to", to}} {
		switch {
		case !filepath.IsAbs(p.v):
			return fmt.Errorf("%w: %s %q is not an absolute path", ErrRerootPaths, p.name, p.v)
		case filepath.Clean(p.v) != p.v:
			return fmt.Errorf("%w: %s %q is not a clean path (%q)", ErrRerootPaths, p.name, p.v, filepath.Clean(p.v))
		case p.v == "/":
			return fmt.Errorf("%w: %s is the filesystem root", ErrRerootPaths, p.name)
		case !utf8.ValidString(p.v):
			return fmt.Errorf("%w: %s %q is not valid UTF-8", ErrRerootPaths, p.name, p.v)
		}
	}
	// Nested trees would carry rows into the tree they came from, or out of the one they
	// are going to: /media -> /media/new rewrites /media/new/a too, to /media/new/new/a.
	if _, ok := RerootPath(to, from, to); ok {
		return fmt.Errorf("%w: to %q lies inside from %q", ErrRerootPaths, to, from)
	}
	if _, ok := RerootPath(from, to, from); ok {
		return fmt.Errorf("%w: from %q lies inside to %q", ErrRerootPaths, from, to)
	}
	return nil
}

// RerootPlan is what carrying the tree at From to To would do to this ledger.
type RerootPlan struct {
	From, To string

	// Tables is every table that holds a path, in the order the rewrite runs, including
	// the ones with nothing to rewrite: "0 rows" is an answer, and a table missing from
	// the report is a question.
	Tables []RerootTable

	// Collisions are rows whose rewritten key is already taken by a row recorded under To.
	// A plan carrying any is refused; nothing here merges two records of one file.
	Collisions []RerootCollision

	// Evidence is every path the rewrite carries for which the ledger records what the
	// file there was, keyed by the path it would be rewritten to, sorted by that path.
	Evidence []RerootEvidence

	rewrites []rerootRewrite
}

// RerootTable is one table's share of a plan.
type RerootTable struct {
	Table string
	// Rows is how many rows have at least one value rewritten.
	Rows int64
	// Columns is every path column of the table and how many values in it are rewritten.
	Columns []RerootColumnCount
}

// RerootColumnCount is how many values one column has under From.
type RerootColumnCount struct {
	Column string
	Values int64
}

// RerootCollision is one row whose rewritten key a row under To already holds.
type RerootCollision struct {
	Table string
	// Was is the row's path as recorded, Path what it would be rewritten to.
	Was, Path string
	// With names the rest of the key where the path is not the whole of it
	// ("fingerprint 1024:1700000000", "sweep_id 3", "epoch 2"), "" where it is.
	With string
}

// RerootEvidence is what the ledger records about the file at one rewritten path.
//
// Exactly one of the two forms is set. A media file carries Fingerprints: the size:mtime
// keys (probe.Fingerprint's form) of every jobs row of that path and of the swap's own
// record of what it left there, so the file the scan would find matches when it matches
// ANY of them - a path with a superseded row beside its current one records two. A
// retained original carries RetainedBytes, the size of the original its retention
// recorded: that file is a second link to the original, and nothing else about it is
// recorded.
type RerootEvidence struct {
	Was, Path     string
	Fingerprints  []string
	RetainedBytes int64
	Retained      bool
}

// Rows is how many rows the plan rewrites across every table.
func (p RerootPlan) Rows() int64 {
	var n int64
	for _, t := range p.Tables {
		n += t.Rows
	}
	return n
}

// rerootRewrite is one value the rewrite writes: the column, the row and the new value.
type rerootRewrite struct {
	col   RerootColumn
	rowid int64
	value string
}

// RerootCollisionsError refuses an apply whose plan collides.
type RerootCollisionsError struct{ Collisions []RerootCollision }

func (e *RerootCollisionsError) Error() string {
	return fmt.Sprintf("store: reroot refused: %d row(s) would take a key a row already holds", len(e.Collisions))
}

// ErrRerootLedgerMoved is an apply whose ledger no longer matches the plan it was asked to
// carry out: the evidence was taken over a different set of rows.
var ErrRerootLedgerMoved = errors.New("store: reroot refused: the ledger no longer matches the plan the evidence was taken over")

// querier is what the plan reads through: the handle for a plan, the transaction's
// connection for the re-plan an apply makes.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// PlanReroot reports what carrying from to to would rewrite, collide with and claim about
// the files at the new paths. It reads and writes nothing.
func (s *SQLite) PlanReroot(ctx context.Context, from, to string) (RerootPlan, error) {
	if err := checkRerootPaths(from, to); err != nil {
		return RerootPlan{From: from, To: to}, err
	}
	return planReroot(ctx, s.db, from, to)
}

// underClause selects the rows whose column is the tree's root or lies beneath it. The
// range is on BINARY collation, a byte comparison: every value beginning root+"/" sorts at
// or after root+"/" and before root+"0", '0' being the byte after '/'. RerootPath decides
// each row again, so this only narrows what is read.
func underClause(col string) string {
	return fmt.Sprintf(`(%[1]s = ? OR (%[1]s >= ? AND %[1]s < ?))`, col)
}

func underArgs(root string) []any { return []any{root, root + "/", root + "0"} }

func planReroot(ctx context.Context, q querier, from, to string) (RerootPlan, error) {
	plan := RerootPlan{From: from, To: to}
	evidence := map[string]*RerootEvidence{}
	for _, t := range rerootTables {
		rows := map[int64]bool{}
		table := RerootTable{Table: t.name}
		for _, c := range t.columns {
			col := RerootColumn{Table: t.name, Column: c}
			n, err := planColumn(ctx, q, col, from, to, &plan, rows)
			if err != nil {
				return plan, err
			}
			table.Columns = append(table.Columns, RerootColumnCount{Column: c, Values: n})
		}
		if t.name == sidecarColumn.Table {
			n, err := planSidecars(ctx, q, from, to, &plan, rows)
			if err != nil {
				return plan, err
			}
			table.Columns = append(table.Columns, RerootColumnCount{Column: sidecarColumn.Column, Values: n})
		}
		table.Rows = int64(len(rows))
		plan.Tables = append(plan.Tables, table)

		if t.key != "" {
			col, err := planCollisions(ctx, q, t, from, to)
			if err != nil {
				return plan, err
			}
			plan.Collisions = append(plan.Collisions, col...)
		}
	}
	if err := planJobsEvidence(ctx, q, from, to, evidence); err != nil {
		return plan, err
	}
	if err := planRetainedEvidence(ctx, q, from, to, evidence); err != nil {
		return plan, err
	}
	for _, e := range evidence {
		sort.Strings(e.Fingerprints)
		plan.Evidence = append(plan.Evidence, *e)
	}
	sort.Slice(plan.Evidence, func(i, j int) bool { return plan.Evidence[i].Path < plan.Evidence[j].Path })
	return plan, nil
}

// planColumn reads one column's values under from and records each rewrite.
func planColumn(ctx context.Context, q querier, col RerootColumn, from, to string, plan *RerootPlan, touched map[int64]bool) (int64, error) {
	rows, err := q.QueryContext(ctx,
		fmt.Sprintf(`SELECT rowid, %s FROM %s WHERE %s ORDER BY rowid`, col.Column, col.Table, underClause(col.Column)),
		underArgs(from)...)
	if err != nil {
		return 0, fmt.Errorf("store: reroot: read %s: %w", col, err)
	}
	defer func() { _ = rows.Close() }()
	var n int64
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			return 0, fmt.Errorf("store: reroot: read %s: %w", col, err)
		}
		nv, ok := RerootPath(v, from, to)
		if !ok {
			continue
		}
		plan.rewrites = append(plan.rewrites, rerootRewrite{col: col, rowid: id, value: nv})
		touched[id] = true
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: reroot: read %s: %w", col, err)
	}
	return n, nil
}

// planSidecars reads every recorded sidecar list and records the rewrite of each list that
// names a sidecar under from. A list the codec cannot read is left exactly as it is: a
// record nothing can interpret is not one this command can carry faithfully.
func planSidecars(ctx context.Context, q querier, from, to string, plan *RerootPlan, touched map[int64]bool) (int64, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT rowid, subtitle_sidecars FROM jobs WHERE subtitle_sidecars IS NOT NULL ORDER BY rowid`)
	if err != nil {
		return 0, fmt.Errorf("store: reroot: read %s: %w", sidecarColumn, err)
	}
	defer func() { _ = rows.Close() }()
	var n int64
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return 0, fmt.Errorf("store: reroot: read %s: %w", sidecarColumn, err)
		}
		recorded := ParseSidecars(text)
		if !recorded.Recorded() {
			continue
		}
		list := recorded.List()
		changed := false
		for i := range list {
			if nv, ok := RerootPath(list[i].Path, from, to); ok && list[i].Path != "" {
				list[i].Path = nv
				changed = true
			}
		}
		if !changed {
			continue
		}
		plan.rewrites = append(plan.rewrites, rerootRewrite{col: sidecarColumn, rowid: id, value: RecordSidecars(list).Encode()})
		touched[id] = true
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: reroot: read %s: %w", sidecarColumn, err)
	}
	return n, nil
}

// planCollisions lists the rows under from whose rewritten key a row under to already
// holds. Every rewritten key lies under to, so the rows recorded there are the only ones
// one can meet; they are read once, not looked up once per row.
func planCollisions(ctx context.Context, q querier, t rerootTable, from, to string) ([]RerootCollision, error) {
	with := "''"
	if t.keyWith != "" {
		with = "CAST(" + t.keyWith + " AS TEXT)"
	}
	read := func(root string) (map[[2]string]bool, [][2]string, error) {
		rows, err := q.QueryContext(ctx,
			fmt.Sprintf(`SELECT %s, %s FROM %s WHERE %s ORDER BY %[1]s`, t.key, with, t.name, underClause(t.key)),
			underArgs(root)...)
		if err != nil {
			return nil, nil, fmt.Errorf("store: reroot: read %s keys: %w", t.name, err)
		}
		defer func() { _ = rows.Close() }()
		set := map[[2]string]bool{}
		var list [][2]string
		for rows.Next() {
			var k [2]string
			if err := rows.Scan(&k[0], &k[1]); err != nil {
				return nil, nil, fmt.Errorf("store: reroot: read %s keys: %w", t.name, err)
			}
			set[k] = true
			list = append(list, k)
		}
		return set, list, rows.Err()
	}
	taken, _, err := read(to)
	if err != nil {
		return nil, err
	}
	if len(taken) == 0 {
		return nil, nil
	}
	_, moving, err := read(from)
	if err != nil {
		return nil, err
	}
	var out []RerootCollision
	for _, k := range moving {
		nv, ok := RerootPath(k[0], from, to)
		if !ok || !taken[[2]string{nv, k[1]}] {
			continue
		}
		c := RerootCollision{Table: t.name, Was: k[0], Path: nv}
		if t.keyWith != "" {
			c.With = t.keyWith + " " + k[1]
		}
		out = append(out, c)
	}
	return out, nil
}

func evidenceAt(evidence map[string]*RerootEvidence, was, path string) *RerootEvidence {
	e := evidence[path]
	if e == nil {
		e = &RerootEvidence{Was: was, Path: path}
		evidence[path] = e
	}
	return e
}

func addFingerprint(e *RerootEvidence, fp string) {
	for _, have := range e.Fingerprints {
		if have == fp {
			return
		}
	}
	e.Fingerprints = append(e.Fingerprints, fp)
}

// planJobsEvidence records the fingerprint of every jobs row the rewrite carries: the
// size:mtime of the file that row was decided about, which is what the scan compares the
// file at the path against.
func planJobsEvidence(ctx context.Context, q querier, from, to string, evidence map[string]*RerootEvidence) error {
	rows, err := q.QueryContext(ctx,
		`SELECT path, fingerprint FROM jobs WHERE `+underClause("path")+` ORDER BY path`, underArgs(from)...)
	if err != nil {
		return fmt.Errorf("store: reroot: read jobs fingerprints: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p, fp string
		if err := rows.Scan(&p, &fp); err != nil {
			return fmt.Errorf("store: reroot: read jobs fingerprints: %w", err)
		}
		if nv, ok := RerootPath(p, from, to); ok {
			addFingerprint(evidenceAt(evidence, p, nv), fp)
		}
	}
	return rows.Err()
}

// planRetainedEvidence records what each retention carried says about its two files: the
// fingerprint the swap left at swapped_path, and the size of the original its retained
// link holds.
func planRetainedEvidence(ctx context.Context, q querier, from, to string, evidence map[string]*RerootEvidence) error {
	rows, err := q.QueryContext(ctx,
		`SELECT swapped_path, swapped_fingerprint, retained_path, source_bytes FROM retained_originals
		 WHERE `+underClause("swapped_path")+` OR `+underClause("retained_path")+` ORDER BY source_path`,
		append(underArgs(from), underArgs(from)...)...)
	if err != nil {
		return fmt.Errorf("store: reroot: read retained originals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var swapped, fp, retained string
		var bytes int64
		if err := rows.Scan(&swapped, &fp, &retained, &bytes); err != nil {
			return fmt.Errorf("store: reroot: read retained originals: %w", err)
		}
		if nv, ok := RerootPath(swapped, from, to); ok {
			addFingerprint(evidenceAt(evidence, swapped, nv), fp)
		}
		if nv, ok := RerootPath(retained, from, to); ok {
			e := evidenceAt(evidence, retained, nv)
			e.Retained, e.RetainedBytes = true, bytes
		}
	}
	return rows.Err()
}

// sameRewrite reports whether two plans rewrite the same values to the same paths.
func (p RerootPlan) sameRewrite(o RerootPlan) bool {
	if p.From != o.From || p.To != o.To || len(p.rewrites) != len(o.rewrites) {
		return false
	}
	for i := range p.rewrites {
		if p.rewrites[i] != o.rewrites[i] {
			return false
		}
	}
	return true
}

// ApplyReroot carries out plan in ONE transaction, and returns the plan it carried out.
//
// The plan is taken again inside the transaction and must rewrite exactly what the
// caller's did, because the caller's evidence was taken over the caller's plan; a ledger
// that moved in between is refused (ErrRerootLedgerMoved), as is one whose plan now
// collides. After the rewrite the plan is taken a third time and must find nothing left
// under From. Any failure on the way rolls back everything: no table is ever left
// rewritten beside one that was not.
//
// No row's updated_at or schema_version is touched. Both describe what decided the row
// and which build wrote it, and a rename decided nothing: re-dating the rows would move
// the oldest-first prune, and re-stamping them would claim this build wrote fields an
// earlier one left empty.
func (s *SQLite) ApplyReroot(ctx context.Context, plan RerootPlan) (RerootPlan, error) {
	if err := checkRerootPaths(plan.From, plan.To); err != nil {
		return plan, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return plan, fmt.Errorf("store: reroot: connect: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// IMMEDIATE, for the reason applyMigration gives: the write lock is taken up front,
	// where a busy handler can wait on it, and the re-plan below reads under it.
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return plan, fmt.Errorf("store: reroot: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	now, err := planReroot(ctx, conn, plan.From, plan.To)
	if err != nil {
		return plan, err
	}
	if !now.sameRewrite(plan) {
		return now, ErrRerootLedgerMoved
	}
	if len(now.Collisions) > 0 {
		return now, &RerootCollisionsError{Collisions: now.Collisions}
	}
	for _, rw := range now.rewrites {
		res, err := conn.ExecContext(ctx,
			fmt.Sprintf(`UPDATE %s SET %s = ? WHERE rowid = ?`, rw.col.Table, rw.col.Column), rw.value, rw.rowid)
		if err != nil {
			return now, fmt.Errorf("store: reroot: rewrite %s: %w", rw.col, err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return now, fmt.Errorf("store: reroot: rewrite %s of row %d touched %d row(s), want 1 (%v)", rw.col, rw.rowid, n, err)
		}
	}
	after, err := planReroot(ctx, conn, plan.From, plan.To)
	if err != nil {
		return now, err
	}
	if left := after.Rows(); left != 0 {
		return now, fmt.Errorf("store: reroot: %d row(s) still name %s after the rewrite; nothing was committed", left, plan.From)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return now, fmt.Errorf("store: reroot: commit: %w", err)
	}
	committed = true
	return now, nil
}
