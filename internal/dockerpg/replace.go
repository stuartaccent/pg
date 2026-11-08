package dockerpg

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"pg/internal/conn"
)

// maintenanceDB is where a replace works from, as a database can't be renamed
// by a connection to it.
const maintenanceDB = "postgres"

var unsafeInDatabase = regexp.MustCompile(`[^A-Za-z0-9_]+`)

// oldName is what a replaced database becomes.
func oldName(database string, at time.Time) string {
	stem := unsafeInDatabase.ReplaceAllString(database, "_")
	if len(stem) > 40 {
		stem = stem[:40]
	}
	return stem + "_old_" + at.Format("20060102_150405")
}

func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

// rows runs a query with text parameters.
func rows(ctx context.Context, c *pgconn.PgConn, sql string, params ...string) ([][][]byte, error) {
	args := make([][]byte, len(params))
	for i, p := range params {
		args[i] = []byte(p)
	}
	res := c.ExecParams(ctx, sql, args, nil, nil, nil).Read()
	return res.Rows, res.Err
}

func execute(ctx context.Context, c *pgconn.PgConn, sql string) error {
	_, err := c.Exec(ctx, sql).ReadAll()
	return err
}

// Replace swaps a database for a restored copy of a backup, and returns what
// the previous one was renamed to, which is kept as the way back.
//
// The copy is built beside the database, under another name, and has to pass
// every check first, so a bad backup never gets near it. Only then do two
// renames swap them, which takes moments, and undo themselves on failure.
// Owners and grants are kept, as the application's roles need them, so a role
// missing here counts as a failure.
//
// shortcut: settings for a role in a database aren't carried over, nor are
// list settings such as search_path quoted properly, and an ICU default
// collation becomes the libc one. Add them if a database ever relies on these.
func Replace(ctx context.Context, t Target, backup, database string, at time.Time, progress func(string)) (Report, string, error) {
	if why := CanReplace(database); why != "" {
		return Report{}, "", errors.New(why)
	}
	image, err := restoreImage(filepath.Base(backup), t.Image)
	if err != nil {
		return Report{}, "", err
	}
	admin, err := conn.Open(ctx, conn.WithDatabase(t.DSN, maintenanceDB))
	if err != nil {
		return Report{}, "", err
	}
	defer admin.Close(context.WithoutCancel(ctx))

	found, err := rows(ctx, admin, `
		select pg_encoding_to_char(encoding), datcollate, datctype, pg_get_userbyid(datdba), datistemplate::text
		from pg_database where datname = $1`, database)
	if err != nil {
		return Report{}, "", err
	}
	if len(found) == 0 {
		return Report{}, "", errors.New("no database called " + database)
	}
	if string(found[0][4]) == "true" {
		return Report{}, "", errors.New("can't replace a template database")
	}

	temp := "pg_replace_" + at.Format("20060102_150405")
	progress("Creating " + temp)
	create := fmt.Sprintf("create database %s template template0 encoding %s lc_collate %s lc_ctype %s", quoteName(temp),
		quoteLiteral(string(found[0][0])), quoteLiteral(string(found[0][1])), quoteLiteral(string(found[0][2])))
	if err := execute(ctx, admin, create); err != nil {
		return Report{}, "", err
	}

	fail := func(err error) (Report, string, error) {
		progress("Removing " + temp)
		if derr := dropCreated(admin, temp); derr != nil {
			return Report{}, "", fmt.Errorf("%w, and removing %s failed too: %v", err, temp, derr)
		}
		return Report{}, "", err
	}

	if err := copyDatabaseSettings(ctx, admin, database, temp, string(found[0][3])); err != nil {
		return fail(err)
	}
	report, err := fill(ctx, t, image, backup, temp, true, progress)
	if err != nil {
		return fail(err)
	}
	if problem := report.Problem(); problem != "" {
		return fail(errors.New(problem + ", so " + database + " was left alone"))
	}
	// Whatever the file's called: swapping one in would leave every table empty.
	if report.SchemaOnly {
		return fail(errors.New(filepath.Base(backup) + " has the schema but no rows, so " + database + " was left alone"))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	progress("Swapping")
	old := oldName(database, at)
	if err := swap(admin, database, temp, old); err != nil {
		return fail(err)
	}
	return report, old, nil
}

// copyDatabaseSettings gives temp what belongs to the database rather than to
// what's in it, which is why a dump doesn't hold it.
func copyDatabaseSettings(ctx context.Context, admin *pgconn.PgConn, database, temp, owner string) error {
	if err := execute(ctx, admin, "alter database "+quoteName(temp)+" owner to "+quoteName(owner)); err != nil {
		return err
	}
	var statements []string
	acl, err := rows(ctx, admin, "select (datacl is not null)::text from pg_database where datname = $1", database)
	if err != nil {
		return err
	}
	if string(acl[0][0]) == "true" {
		statements = append(statements, "revoke all on database "+quoteName(temp)+" from public")
	}
	for _, query := range []string{`
		select format('grant %s on database %I to %s', a.privilege_type, $2::text,
			case when a.grantee = 0 then 'public' else quote_ident(pg_get_userbyid(a.grantee)) end)
		from pg_database d, aclexplode(d.datacl) a
		where d.datname = $1`, `
		select format('alter database %I set %I = %L', $2::text, split_part(c, '=', 1), substr(c, strpos(c, '=') + 1))
		from pg_db_role_setting s
		join pg_database d on d.oid = s.setdatabase
		cross join unnest(s.setconfig) c
		where d.datname = $1 and s.setrole = 0`} {
		found, err := rows(ctx, admin, query, database, temp)
		if err != nil {
			return err
		}
		for _, row := range found {
			statements = append(statements, string(row[0]))
		}
	}
	for _, statement := range statements {
		if err := execute(ctx, admin, statement); err != nil {
			return err
		}
	}
	return nil
}

// swap renames database to old and temp to database, closing connections to it
// first. Refusing new ones stops an application reconnecting in the gap. It
// isn't tied to the job's context: once started it has to finish or undo.
func swap(admin *pgconn.PgConn, database, temp, old string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	allow := func(name string, allowed bool) error {
		return execute(ctx, admin, fmt.Sprintf("alter database %s allow_connections %t", quoteName(name), allowed))
	}
	if err := allow(database, false); err != nil {
		return err
	}
	if _, err := rows(ctx, admin, "select pg_terminate_backend(pid) from pg_stat_activity where datname = $1", database); err != nil {
		return errors.Join(err, allow(database, true))
	}
	if err := execute(ctx, admin, "alter database "+quoteName(database)+" rename to "+quoteName(old)); err != nil {
		return errors.Join(err, allow(database, true))
	}
	if err := execute(ctx, admin, "alter database "+quoteName(temp)+" rename to "+quoteName(database)); err != nil {
		undo := execute(ctx, admin, "alter database "+quoteName(old)+" rename to "+quoteName(database))
		return errors.Join(err, undo, allow(database, true))
	}
	return allow(old, true)
}

// CanReplace reports why database can't be replaced, or "" if it can.
func CanReplace(database string) string {
	if slices.Contains([]string{maintenanceDB, "template0", "template1"}, database) {
		return "can't replace " + database
	}
	return ""
}
