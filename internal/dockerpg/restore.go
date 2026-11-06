package dockerpg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"pg/internal/conn"
)

// Target is the server a backup is restored to.
type Target struct {
	DSN      string
	Settings conn.Settings
	Image    string // the image matching the server
}

var validDatabase = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// ValidDatabase reports whether name is one pg will create. It's narrower than
// Postgres allows, as a name that needs thought isn't worth the risk here.
func ValidDatabase(name string) bool {
	return validDatabase.MatchString(name)
}

// RestoreName suggests a database name for restoring a backup.
func RestoreName(backupName string, at time.Time) string {
	stem := strings.TrimSuffix(backupName, ".backup")
	if loc := versionInName.FindStringIndex(stem); loc != nil {
		stem = stem[:loc[0]]
	}
	stem = regexp.MustCompile(`[^A-Za-z0-9_]+`).ReplaceAllString(stem, "_")
	if stem == "" || stem[0] >= '0' && stem[0] <= '9' {
		stem = "db_" + stem
	}
	if len(stem) > 44 {
		stem = stem[:44]
	}
	return stem + "_restored_" + at.Format("20060102")
}

// restoreImage picks the pg_restore to use: one at least as new as the backup,
// which is the server's unless the backup's origin isn't known. Postgres
// doesn't support loading a dump into an older server.
func restoreImage(backupName, serverImage string) (string, error) {
	m := versionInName.FindStringSubmatch(backupName)
	if m == nil {
		return "postgres:latest", nil
	}
	backup, _ := strconv.Atoi(m[1])
	server, err := strconv.Atoi(strings.TrimPrefix(serverImage, "postgres:"))
	if err != nil {
		return "postgres:latest", nil
	}
	if backup > server {
		return "", fmt.Errorf("this backup is from Postgres %d, and the server is %d", backup, server)
	}
	return serverImage, nil
}

func quoteName(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Restore restores a backup into a new database on target, then checks every
// table it lists arrived. Owners and grants are left out, so everything belongs
// to the connected user.
//
// It only ever drops a database it created itself, so a failed restore leaves
// nothing behind, and an existing database can't be harmed: creating one with
// the same name fails before anything else happens.
func Restore(ctx context.Context, t Target, backup, database string, progress func(string)) (Report, error) {
	if !ValidDatabase(database) {
		return Report{}, errors.New("not a usable database name: " + database)
	}
	image, err := restoreImage(filepath.Base(backup), t.Image)
	if err != nil {
		return Report{}, err
	}

	admin, err := conn.Open(ctx, t.DSN)
	if err != nil {
		return Report{}, err
	}
	defer admin.Close(context.WithoutCancel(ctx))

	progress("Creating " + database)
	// template0, as a dump expects to start from nothing.
	if _, err := admin.Exec(ctx, "create database "+quoteName(database)+" template template0").ReadAll(); err != nil {
		return Report{}, err
	}

	report, err := fill(ctx, t, image, backup, database, progress)
	if err == nil && len(report.Counts) != report.ListedTables {
		err = errors.New(Report{ListedTables: report.ListedTables, Counts: report.Counts}.Problem())
	}
	if err != nil {
		progress("Removing " + database)
		if derr := dropCreated(admin, database); derr != nil {
			return Report{}, fmt.Errorf("%w, and removing %s failed too: %v", err, database, derr)
		}
		return Report{}, err
	}
	return report, nil
}

// dropCreated removes a database Restore made. It has its own context, as it
// has to work after the restore's has been cancelled.
func dropCreated(admin *pgconn.PgConn, database string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := admin.ExecParams(ctx, "select pg_terminate_backend(pid) from pg_stat_activity where datname = $1",
		[][]byte{[]byte(database)}, nil, nil, nil).Read()
	if res.Err != nil {
		return res.Err
	}
	_, err := admin.Exec(ctx, "drop database "+quoteName(database)).ReadAll()
	return err
}

// fill loads the backup into database and reads back what arrived.
func fill(ctx context.Context, t Target, image, backup, database string, progress func(string)) (Report, error) {
	f, err := os.Open(backup)
	if err != nil {
		return Report{}, err
	}
	defer f.Close()

	progress("Restoring " + filepath.Base(backup))
	env := []string{
		"PGHOST=" + containerHost(t.Settings.Host),
		"PGPORT=" + strconv.Itoa(int(t.Settings.Port)),
		"PGUSER=" + t.Settings.User,
		"PGPASSWORD=" + t.Settings.Password,
	}
	restore := command(ctx, env, "run", "--rm", "--init", "--interactive",
		"-e", "PGHOST", "-e", "PGPORT", "-e", "PGUSER", "-e", "PGPASSWORD",
		image, "pg_restore", "--no-owner", "--no-privileges", "--verbose", "--dbname="+database)
	restore.Stdin = f
	var ignored int
	last, err := run(restore, func(line string) {
		if m := ignoredErrors.FindStringSubmatch(line); m != nil {
			ignored, _ = strconv.Atoi(m[1])
		}
		progress(line)
	})
	// pg_restore exits non-zero for skipped errors too, which it counts for us.
	if err != nil && (ignored == 0 || ctx.Err() != nil) {
		return Report{}, fmt.Errorf("restore failed: %w: %s", err, strings.Join(last, " "))
	}

	if _, err := f.Seek(0, 0); err != nil {
		return Report{}, err
	}
	progress("Checking what arrived")
	var list, stderr bytes.Buffer
	toc := command(ctx, nil, "run", "--rm", "--init", "--interactive", image, "pg_restore", "--list")
	toc.Stdin, toc.Stdout, toc.Stderr = f, &list, &stderr
	if err := toc.Run(); err != nil {
		return Report{}, fmt.Errorf("reading the backup: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	c, err := conn.Open(ctx, conn.WithDatabase(t.DSN, database))
	if err != nil {
		return Report{}, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	counts, err := TableCounts(ctx, c)
	if err != nil {
		return Report{}, err
	}
	return Report{ListedTables: countListedTables(list.String()), Counts: counts, Ignored: ignored}, nil
}
