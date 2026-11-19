// Package conn lists the services in pg_service.conf and opens connections to them.
//
// Connection details and passwords come from the standard Postgres files
// (~/.pg_service.conf and ~/.pgpass, or their equivalents in
// %APPDATA%\postgresql on Windows), so the app stores no credentials itself.
package conn

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgservicefile"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// ServiceFilePath returns the service file libpq would read: $PGSERVICEFILE,
// falling back to ~/.pg_service.conf, or %APPDATA%\postgresql\.pg_service.conf
// on Windows.
func ServiceFilePath() (string, error) {
	if p := os.Getenv("PGSERVICEFILE"); p != "" {
		return p, nil
	}
	return defaultServiceFile()
}

// PassFilePath returns the password file libpq would read: $PGPASSFILE, falling
// back to ~/.pgpass, or %APPDATA%\postgresql\pgpass.conf on Windows.
func PassFilePath() (string, error) {
	if p := os.Getenv("PGPASSFILE"); p != "" {
		return p, nil
	}
	return defaultPassFile()
}

// ExposedPassFile returns the path of the password file if anyone but its owner
// can read it, otherwise "". libpq ignores such a file, but pgx uses it, so
// nothing else would mention it.
func ExposedPassFile() string {
	path, err := PassFilePath()
	if err != nil {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !exposed(info) {
		return ""
	}
	return path
}

// EnsureFile creates path if it's missing, readable only by its owner, as
// libpq ignores a .pgpass that anyone else can read. An existing file is left
// exactly as it is. Its folder is made too, as %APPDATA%\postgresql may not
// be there yet.
func EnsureFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// PasswordRejected reports whether err is the server turning down a password,
// which includes there being none to send.
func PasswordRejected(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == invalidPassword
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

// ServiceDSN returns the connection string for a service.
func ServiceDSN(service string) string {
	return "service=" + quote(service)
}

// WithDatabase returns dsn pointed at another database on the same server. A
// later setting overrides an earlier one, and a service's own.
func WithDatabase(dsn, database string) string {
	return dsn + " dbname=" + quote(database)
}

// Open opens a connection.
//
// Cancelling a context passed to a query on the returned connection sends a
// cancel request to the server, so the running statement stops but the
// connection survives. (pgconn's default instead drops the connection.)
func Open(ctx context.Context, dsn string) (*pgconn.PgConn, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// Tells pg's own connections apart in the activity list.
	if cfg.RuntimeParams["application_name"] == "" {
		cfg.RuntimeParams["application_name"] = "pg"
	}
	cfg.OnNotice = func(c *pgconn.PgConn, n *pgconn.Notice) {
		c.CustomData()[noticesKey] = append(TakeNotices(c), noticeText(n))
	}
	cfg.BuildContextWatcherHandler = func(c *pgconn.PgConn) ctxwatch.Handler {
		// If the server hasn't answered the cancel request within DeadlineDelay,
		// fall back to breaking the connection so the UI never hangs.
		return &pgconn.CancelRequestContextWatcherHandler{Conn: c, DeadlineDelay: 3 * time.Second}
	}
	return pgconn.ConnectConfig(ctx, cfg)
}

const noticesKey = "notices"

// TakeNotices returns the notices c has received since last asked, such as
// RAISE NOTICE's, and forgets them.
func TakeNotices(c *pgconn.PgConn) []string {
	notices, _ := c.CustomData()[noticesKey].([]string)
	delete(c.CustomData(), noticesKey)
	return notices
}

// noticeText lays a notice out as psql does.
func noticeText(n *pgconn.Notice) string {
	text := n.Severity + ":  " + n.Message
	if n.Detail != "" {
		text += "\nDETAIL:  " + n.Detail
	}
	if n.Hint != "" {
		text += "\nHINT:  " + n.Hint
	}
	return text
}

// Alive reports whether c still works. Asking the server is the only way to
// know: a connection that was ended, by a restart say, looks fine until used.
func Alive(ctx context.Context, c *pgconn.PgConn) bool {
	return !c.IsClosed() && c.Ping(ctx) == nil
}

// Settings are the details of a connection, for handing to another program.
type Settings struct {
	Host     string
	Port     uint16
	User     string
	Password string
	Database string
	SSL      []string // libpq's variables for the service's ssl settings, such as PGSSLMODE=verify-full
}

// SSLVars returns libpq's variables for a service's ssl settings, such as
// PGSSLMODE=verify-full and PGSSLROOTCERT, so another program connects as
// securely as pg does. Each is the setting's name, upper case without
// underscores, after PG.
func SSLVars(service string) ([]string, error) {
	path, err := ServiceFilePath()
	if err != nil {
		return nil, err
	}
	sf, err := pgservicefile.ReadServicefile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	svc, err := sf.GetService(service)
	if err != nil {
		return nil, err
	}
	var vars []string
	for name, value := range svc.Settings {
		if strings.HasPrefix(name, "ssl") {
			vars = append(vars, "PG"+strings.ToUpper(strings.ReplaceAll(name, "_", ""))+"="+value)
		}
	}
	slices.Sort(vars)
	return vars, nil
}

// SettingsFor resolves dsn, including any service and password files. With no
// database named, the server uses the one named after the user, so that's it.
func SettingsFor(dsn string) (Settings, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return Settings{}, err
	}
	return Settings{Host: cfg.Host, Port: cfg.Port, User: cfg.User, Password: cfg.Password, Database: cmp.Or(cfg.Database, cfg.User)}, nil
}

// ReadOnly reports whether c defaults to read-only transactions, which a
// service asks for with options=-c default_transaction_read_only=on. Servers
// before Postgres 14 don't report the setting, so it reads as false there.
func ReadOnly(c *pgconn.PgConn) bool {
	return c.ParameterStatus("default_transaction_read_only") == "on"
}

// Export streams the rows of query to w as CSV and returns how many there were.
//
// Exporting has to run the query again, so it does so in a read-only
// transaction: one that writes fails, rather than writing twice. It leaves the
// transaction open, so c is for closing afterwards.
func Export(ctx context.Context, c *pgconn.PgConn, query string, w io.Writer) (int64, error) {
	// Splitting drops a final semicolon and comments after it, which can't go
	// inside the brackets of a copy. SQL it can't scan is left for the server.
	if stmts, err := pg_query.SplitWithScanner(query, true); err == nil {
		if len(stmts) > 1 {
			return 0, errors.New("export runs one query at a time: select just the one to export")
		}
		if len(stmts) == 1 {
			query = stmts[0]
		}
	}
	if _, err := c.Exec(ctx, "begin read only").ReadAll(); err != nil {
		return 0, err
	}
	// The newline stops a trailing -- comment swallowing the closing bracket.
	tag, err := c.CopyTo(ctx, w, "copy (\n"+query+"\n) to stdout with (format csv, header)")
	return tag.RowsAffected(), err
}

// invalidPassword is the SQLSTATE for a rejected password.
const invalidPassword = "28P01"

// quote quotes a value for a keyword/value connection string.
func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}
