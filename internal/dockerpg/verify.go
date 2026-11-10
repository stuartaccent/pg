package dockerpg

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"pg/internal/conn"
)

// Report is what restoring a backup showed.
type Report struct {
	ListedTables int              // tables in the backup's table of contents
	Counts       map[string]int64 // rows in each table that was restored
	Ignored      int              // errors the restore skipped over
	FirstError   string           // the first of them, if known
	// SchemaOnly is set when the backup lists tables but none of their rows,
	// as a schema-only dump does.
	SchemaOnly bool
}

// Rows returns the total rows restored.
func (r Report) Rows() int64 {
	var total int64
	for _, n := range r.Counts {
		total += n
	}
	return total
}

// Problem says what's wrong with the backup, or "" if nothing is.
func (r Report) Problem() string {
	switch {
	case len(r.Counts) != r.ListedTables:
		return fmt.Sprintf("the backup lists %d tables, but %d were restored", r.ListedTables, len(r.Counts))
	case r.Ignored > 0 && r.FirstError != "":
		return fmt.Sprintf("the restore skipped %d errors, the first being: %s", r.Ignored, r.FirstError)
	case r.Ignored > 0:
		return fmt.Sprintf("the restore skipped %d errors", r.Ignored)
	}
	return ""
}

var listedTable = regexp.MustCompile(`(?m)^\d+;\s+\d+\s+\d+\s+TABLE\s+(\S+)`)

// countListedTables counts the tables in pg_restore --list's output, and
// whether any of their rows are there. Entries are "TABLE schema name", beside
// "TABLE DATA" for a table's rows and "TABLE ATTACH" for a partition, which
// aren't tables of their own.
func countListedTables(list string) (tables int, rows bool) {
	for _, match := range listedTable.FindAllStringSubmatch(list, -1) {
		switch match[1] {
		case "DATA":
			rows = true
		case "ATTACH":
		default:
			tables++
		}
	}
	return tables, rows
}

// Verify proves a backup can be used: it restores it into a throwaway sandbox,
// checks every table it lists arrived and counts their rows. The sandbox never
// outlives this, whatever happens.
func Verify(ctx context.Context, image, backup string, progress func(string)) (Report, error) {
	sb, skipped, err := NewSandbox(ctx, image, backup, progress)
	if err != nil {
		return Report{}, err
	}
	defer removeLater(sb.Name)
	list := command(ctx, nil, "exec", "--interactive", sb.Name, "pg_restore", "--list")
	return readBack(ctx, list, backup, sb.DSN(), skipped, progress)
}

// readBack reports what a restore left in dsn. list is a pg_restore --list,
// given the backup to read the table of contents from.
func readBack(ctx context.Context, list *exec.Cmd, backup, dsn string, skipped Skipped, progress func(string)) (Report, error) {
	f, err := os.Open(backup)
	if err != nil {
		return Report{}, err
	}
	defer f.Close()
	progress("Checking what arrived")
	var contents, stderr bytes.Buffer
	list.Stdin, list.Stdout, list.Stderr = f, &contents, &stderr
	if err := list.Run(); err != nil {
		return Report{}, fmt.Errorf("reading the backup: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	c, err := conn.Open(ctx, dsn)
	if err != nil {
		return Report{}, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	counts, err := TableCounts(ctx, c)
	if err != nil {
		return Report{}, err
	}
	tables, rows := countListedTables(contents.String())
	return Report{ListedTables: tables, Counts: counts, Ignored: skipped.Count, FirstError: skipped.First, SchemaOnly: tables > 0 && !rows}, nil
}

// TableCounts returns the exact rows in every table, by schema-qualified name.
// Tables belonging to extensions are left out, as backups don't list them.
func TableCounts(ctx context.Context, c *pgconn.PgConn) (map[string]int64, error) {
	res := c.ExecParams(ctx, `
		select format('%I.%I', n.nspname, c.relname),
			(xpath('/row/c/text()', query_to_xml(format('select count(*) as c from only %I.%I', n.nspname, c.relname), false, true, '')))[1]::text
		from pg_class c
		join pg_namespace n on n.oid = c.relnamespace
		where c.relkind in ('r', 'p')
			and n.nspname !~ '^pg_' and n.nspname <> 'information_schema'
			and not exists (select 1 from pg_depend d
				where d.classid = 'pg_class'::regclass and d.objid = c.oid and d.deptype = 'e')`, nil, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, res.Err
	}
	counts := make(map[string]int64, len(res.Rows))
	for _, row := range res.Rows {
		n, err := strconv.ParseInt(string(row[1]), 10, 64)
		if err != nil {
			return nil, err
		}
		counts[string(row[0])] = n
	}
	return counts, nil
}

// Differences lists how a backup's tables differ from a live database's.
func Differences(backup, live map[string]int64) []string {
	var found []string
	for table, rows := range backup {
		liveRows, ok := live[table]
		switch {
		case !ok:
			found = append(found, table+": no longer in the live database")
		case rows != liveRows:
			found = append(found, fmt.Sprintf("%s: %d rows in the backup, %d live", table, rows, liveRows))
		}
	}
	for table := range live {
		if _, ok := backup[table]; !ok {
			found = append(found, table+": not in the backup")
		}
	}
	slices.Sort(found)
	return found
}

// SameDatabase reports whether a backup made by pg came from database: all of
// the name before the version, so app doesn't match app-pg_audit's backups.
func SameDatabase(backupName, database string) bool {
	loc := versionInName.FindStringIndex(backupName)
	return database != "" && loc != nil && backupName[:loc[0]] == unsafeInName.ReplaceAllString(database, "_")
}
