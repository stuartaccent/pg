package catalog

import (
	"context"
	"embed"
	"io/fs"
	"strings"
)

// Each check is a query in health/, named for the order they're shown in, and
// listing what it found. Its leading comment says why that matters.
//
//go:embed health
var healthChecks embed.FS

// Check is the outcome of one health check.
type Check struct {
	Name   string
	Reason string
	// Usage is set for checks that go by usage statistics, which only mean
	// much on a server that has been in use since they began.
	Usage   bool
	SQL     string
	Columns []string   // what the check returns, even with no rows
	Rows    [][]string // NULL comes through as ""
	Err     error
}

// Report is the outcome of every health check.
type Report struct {
	// StatsSince is when usage statistics were last reset, or "" if never, in
	// which case they run from when the database was made or restored.
	StatsSince string
	Checks     []Check
}

// Health runs every check against the connected database. One failing, for
// want of a privilege say, doesn't stop the others.
func (c *Catalog) Health(ctx context.Context) (Report, error) {
	var report Report
	rows, err := c.read(ctx, `select to_char(stats_reset, 'YYYY-MM-DD HH24:MI') from pg_stat_database where datname = current_database()`, nil)
	if err != nil {
		return report, err
	}
	if len(rows) > 0 {
		report.StatsSince = string(rows[0][0])
	}

	entries, err := fs.ReadDir(healthChecks, "health")
	if err != nil {
		return report, err
	}
	for _, e := range entries {
		sql, err := fs.ReadFile(healthChecks, "health/"+e.Name())
		if err != nil {
			return report, err
		}
		check := newCheck(e.Name(), string(sql))
		res, err := c.query(ctx, check.SQL, nil)
		if err != nil {
			check.Err = err
		} else {
			for _, f := range res.FieldDescriptions {
				check.Columns = append(check.Columns, f.Name)
			}
			for _, r := range res.Rows {
				row := make([]string, len(r))
				for i, v := range r {
					row[i] = string(v)
				}
				check.Rows = append(check.Rows, row)
			}
		}
		report.Checks = append(report.Checks, check)
	}
	return report, nil
}

// newCheck reads a check's name from its file name, after the number that
// orders it, and its reason from the comment it starts with.
func newCheck(file, sql string) Check {
	_, name, _ := strings.Cut(strings.TrimSuffix(file, ".sql"), " ")
	var reason []string
	for line := range strings.Lines(sql) {
		comment, ok := strings.CutPrefix(line, "--")
		if !ok {
			break
		}
		reason = append(reason, strings.TrimSpace(comment))
	}
	text, usage := strings.CutPrefix(strings.Join(reason, " "), "Usage: ")
	if text != "" {
		text = strings.ToUpper(text[:1]) + text[1:]
	}
	return Check{Name: name, Reason: text, Usage: usage, SQL: sql}
}
