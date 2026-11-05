// Package conn lists the services in pg_service.conf and opens connections to them.
//
// Connection details and passwords come from the standard Postgres files
// (~/.pg_service.conf and ~/.pgpass), so the app stores no credentials itself.
package conn

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgservicefile"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
)

// ServiceFilePath returns the service file libpq would read: $PGSERVICEFILE,
// falling back to ~/.pg_service.conf.
func ServiceFilePath() (string, error) {
	if p := os.Getenv("PGSERVICEFILE"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pg_service.conf"), nil
}

// Services returns the sorted service names. A missing service file is not an
// error; it just means there are no services yet.
func Services() ([]string, error) {
	path, err := ServiceFilePath()
	if err != nil {
		return nil, err
	}
	sf, err := pgservicefile.ReadServicefile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	names := make([]string, 0, len(sf.Services))
	for _, s := range sf.Services {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	return names, nil
}

// Connect opens a connection to the named service.
//
// Cancelling a context passed to a query on the returned connection sends a
// cancel request to the server, so the running statement stops but the
// connection survives. (pgconn's default instead drops the connection.)
func Connect(ctx context.Context, service string) (*pgconn.PgConn, error) {
	cfg, err := pgconn.ParseConfig("service=" + quote(service))
	if err != nil {
		return nil, err
	}
	cfg.BuildContextWatcherHandler = func(c *pgconn.PgConn) ctxwatch.Handler {
		// If the server hasn't answered the cancel request within DeadlineDelay,
		// fall back to breaking the connection so the UI never hangs.
		return &pgconn.CancelRequestContextWatcherHandler{Conn: c, DeadlineDelay: 3 * time.Second}
	}
	return pgconn.ConnectConfig(ctx, cfg)
}

// quote quotes a value for a keyword/value connection string.
func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}
